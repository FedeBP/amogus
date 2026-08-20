package main

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Per-guild playback state
// ---------------------------------------------------------------------------

// guildState holds all mutable playback state for one Discord server.
// Keeping state per-guild ensures that multiple servers never interfere.
type guildState struct {
	mu                   sync.Mutex
	actionQueue          chan guildAction
	songQueue            []Song
	isPlaying            bool
	autoplayEnabled      bool
	lastPlayed           Track
	lastVoiceChannelID   string
	playedVideoIDs       map[string]struct{}
	disconnectTimer      *time.Timer
	activeDlCmd          *exec.Cmd
	activeFfCmd          *exec.Cmd
	nowPlayingChannelID  string
	nowPlayingMessageID  string
	nowPlayingControlID  string
	nowPlayingControlSeq uint64
	// skipInterrupt is set to true by /skip to signal streamAudio to halt.
	skipInterrupt      atomic.Bool
	playbackGeneration atomic.Uint64
	// intentionalLeave prevents voiceStateHandler from treating a
	// programmatic disconnect as an external kick.
	intentionalLeave atomic.Bool
}

// killPlaybackProcesses kills the active yt-dlp and ffmpeg processes, if any.
// The caller must NOT hold gs.mu when calling this — it acquires the lock itself.
func (gs *guildState) killPlaybackProcesses() {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.activeDlCmd != nil && gs.activeDlCmd.Process != nil {
		_ = gs.activeDlCmd.Process.Kill()
	}
	if gs.activeFfCmd != nil && gs.activeFfCmd.Process != nil {
		_ = gs.activeFfCmd.Process.Kill()
	}
	gs.activeDlCmd = nil
	gs.activeFfCmd = nil
}

// interruptPlayback signals the streaming loop to stop the current track
// and kills the underlying processes immediately.
func (gs *guildState) interruptPlayback() {
	gs.skipInterrupt.Store(true)
	gs.killPlaybackProcesses()
}

func (gs *guildState) stopAfterAutoplayError(generation uint64) bool {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.playbackGeneration.Load() != generation {
		return true
	}
	if len(gs.songQueue) > 0 {
		return false
	}
	gs.isPlaying = false
	return true
}

func (gs *guildState) disableAutoplayAfterPlaybackError(generation uint64) bool {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.playbackGeneration.Load() != generation {
		return false
	}
	wasEnabled := gs.autoplayEnabled
	gs.autoplayEnabled = false
	return wasEnabled
}

func newGuildState() *guildState {
	gs := &guildState{
		actionQueue: make(chan guildAction, guildActionQueueSize),
	}
	safeGo("guild action queue", gs.runActions)
	return gs
}

func (gs *guildState) runActions() {
	for action := range gs.actionQueue {
		safeRun("guild action", action)
	}
}

func (gs *guildState) enqueueAction(action guildAction) bool {
	if gs.actionQueue == nil {
		action()
		return true
	}
	select {
	case gs.actionQueue <- action:
		return true
	default:
		return false
	}
}

type nowPlayingMessageRef struct {
	channelID string
	messageID string
}

func (gs *guildState) activateNowPlayingControls(channelID string) (string, nowPlayingMessageRef) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	previous := nowPlayingMessageRef{
		channelID: gs.nowPlayingChannelID,
		messageID: gs.nowPlayingMessageID,
	}
	gs.nowPlayingControlSeq++
	controlID := fmt.Sprintf("%d", gs.nowPlayingControlSeq)
	gs.nowPlayingChannelID = channelID
	gs.nowPlayingMessageID = ""
	gs.nowPlayingControlID = controlID
	return controlID, previous
}

func (gs *guildState) confirmNowPlayingMessage(controlID, messageID string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.nowPlayingControlID != controlID {
		return
	}
	gs.nowPlayingMessageID = messageID
}

func (gs *guildState) clearNowPlayingControlsFor(controlID string) nowPlayingMessageRef {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if controlID != "" && gs.nowPlayingControlID != controlID {
		return nowPlayingMessageRef{}
	}
	return gs.clearNowPlayingControlsLocked()
}

func (gs *guildState) clearNowPlayingControlsLocked() nowPlayingMessageRef {
	previous := nowPlayingMessageRef{
		channelID: gs.nowPlayingChannelID,
		messageID: gs.nowPlayingMessageID,
	}
	gs.nowPlayingChannelID = ""
	gs.nowPlayingMessageID = ""
	gs.nowPlayingControlID = ""
	return previous
}

func (gs *guildState) activeNowPlayingAction(customID string) (string, bool) {
	action, controlID, ok := parseNowPlayingCustomID(customID)
	if !ok {
		return "", false
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return action, controlID != "" && controlID == gs.nowPlayingControlID
}

func parseNowPlayingCustomID(customID string) (string, string, bool) {
	i := strings.LastIndex(customID, ":")
	if i <= 0 || i == len(customID)-1 {
		return "", "", false
	}
	action := customID[:i]
	switch action {
	case nowPlayingButtonSkip, nowPlayingButtonStop, nowPlayingButtonQueue, nowPlayingButtonAutoplay:
		return action, customID[i+1:], true
	default:
		return "", "", false
	}
}

type autoplaySkipRequest struct {
	seed           Track
	voiceChannelID string
	playedVideoIDs map[string]struct{}
}

func (gs *guildState) prepareSkip() (autoplaySkipRequest, bool) {
	gs.mu.Lock()
	autoplaySkip := gs.isPlaying &&
		gs.autoplayEnabled &&
		len(gs.songQueue) == 0 &&
		trackVideoID(gs.lastPlayed) != "" &&
		gs.lastVoiceChannelID != ""
	req := autoplaySkipRequest{}
	if autoplaySkip {
		req = autoplaySkipRequest{
			seed:           gs.lastPlayed,
			voiceChannelID: gs.lastVoiceChannelID,
			playedVideoIDs: gs.playedVideoIDsLocked(),
		}
	}
	gs.mu.Unlock()

	if !autoplaySkip {
		gs.interruptPlayback()
		return autoplaySkipRequest{}, false
	}

	gs.playbackGeneration.Add(1)
	gs.interruptPlayback()
	gs.mu.Lock()
	gs.isPlaying = false
	if gs.disconnectTimer != nil {
		gs.disconnectTimer.Stop()
		gs.disconnectTimer = nil
	}
	gs.mu.Unlock()
	return req, true
}

// resetPlaybackState invalidates the current playback generation, clears queue
// and autoplay state, kills active processes, and cancels idle disconnects.
func (gs *guildState) resetPlaybackState() {
	gs.playbackGeneration.Add(1)
	gs.killPlaybackProcesses()
	gs.skipInterrupt.Store(false)
	gs.mu.Lock()
	gs.songQueue = nil
	gs.isPlaying = false
	gs.autoplayEnabled = false
	gs.lastPlayed = Track{}
	gs.lastVoiceChannelID = ""
	gs.playedVideoIDs = nil
	gs.clearNowPlayingControlsLocked()
	if gs.disconnectTimer != nil {
		gs.disconnectTimer.Stop()
		gs.disconnectTimer = nil
	}
	gs.mu.Unlock()
}

type voiceLeaveRecovery struct {
	textChannelID string
	controls      nowPlayingMessageRef
	generation    uint64
}

func (gs *guildState) prepareUnexpectedVoiceLeaveRecovery(guildID string) (voiceLeaveRecovery, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	if !gs.isPlaying ||
		gs.lastVoiceChannelID == "" ||
		gs.nowPlayingChannelID == "" ||
		gs.lastPlayed.URL == "" {
		return voiceLeaveRecovery{}, false
	}

	current := Song{
		guildID:   guildID,
		channelID: gs.lastVoiceChannelID,
		track:     gs.lastPlayed,
	}
	gs.songQueue = append([]Song{current}, gs.songQueue...)
	generation := gs.playbackGeneration.Add(1)
	if gs.disconnectTimer != nil {
		gs.disconnectTimer.Stop()
		gs.disconnectTimer = nil
	}
	textChannelID := gs.nowPlayingChannelID
	controls := gs.clearNowPlayingControlsLocked()

	return voiceLeaveRecovery{
		textChannelID: textChannelID,
		controls:      controls,
		generation:    generation,
	}, true
}

// Guild-state registry.
var (
	guildsMu sync.Mutex
	guilds   = make(map[string]*guildState)
)

// getGuildState returns the guildState for guildID, creating one if absent.
func getGuildState(guildID string) *guildState {
	guildsMu.Lock()
	defer guildsMu.Unlock()
	if gs, ok := guilds[guildID]; ok {
		return gs
	}
	gs := newGuildState()
	guilds[guildID] = gs
	return gs
}
