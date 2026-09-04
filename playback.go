package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"log"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Play flow
// ---------------------------------------------------------------------------

// runPlayFlow resolves a /play query as either a playlist or a single track,
// enqueues the result, and returns the status text for the interaction. An
// empty status means the caller should use the standard single-track queued
// response.
func runPlayFlow(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, query string) string {
	if strings.Contains(query, "list=") {
		_, _ = s.ChannelMessageSend(textChannelID, "Resolving playlist…")
		n, err := fetchPlaylistEnqueue(ctx, s, guildID, textChannelID, voiceChannelID, query)
		switch {
		case errors.Is(err, errPlaylistMaxSize):
			return fmt.Sprintf("Queued %d tracks (limit is %d per playlist).", n, maxPlaylistTracks)
		case err != nil && n > 0:
			return fmt.Sprintf("Loaded %d tracks, then hit an error: %v", n, err)
		case err != nil:
			log.Printf("playlist: %v", err)
			return fmt.Sprintf("Could not load playlist: %v", err)
		case n == 0:
			return "That playlist has no playable videos."
		default:
			return fmt.Sprintf("Queued %d tracks.", n)
		}
	}

	track, err := resolveTrack(ctx, query)
	if err != nil {
		return fmt.Sprintf("Could not find a track: %v", err)
	}
	_ = enqueueTrack(s, guildID, textChannelID, voiceChannelID, track)
	return ""
}

// enqueueTrack appends a track to the guild queue and starts the playback
// goroutine if nothing is currently playing.
func enqueueTrack(s *discordgo.Session, guildID, textChannelID, voiceChannelID string, track Track) error {
	gs := getGuildState(guildID)

	gs.mu.Lock()
	gs.songQueue = append(gs.songQueue, Song{
		guildID:   guildID,
		channelID: voiceChannelID,
		track:     track,
	})
	shouldStart := !gs.isPlaying
	generation := gs.playbackGeneration.Load()
	if shouldStart {
		gs.isPlaying = true
		if gs.disconnectTimer != nil {
			gs.disconnectTimer.Stop()
			gs.disconnectTimer = nil
		}
	}
	gs.mu.Unlock()

	if shouldStart {
		safeGo("playback", func() {
			playNextSong(s, guildID, textChannelID, generation)
		})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Playback loop
// ---------------------------------------------------------------------------

// playNextSong is the main playback loop for a guild. It processes queued
// tracks, optionally extends the queue through autoplay, and exits when the
// playback generation changes or no more work remains.
func playNextSong(s *discordgo.Session, guildID, textChannelID string, generation uint64) {
	gs := getGuildState(guildID)

	dl, err := ytDownloaderPath()
	if err != nil {
		log.Printf("downloader: %v", err)
		_, _ = s.ChannelMessageSend(textChannelID, "yt-dlp must be installed and on PATH.")
		gs.mu.Lock()
		if gs.playbackGeneration.Load() == generation {
			gs.isPlaying = false
		}
		gs.mu.Unlock()
		return
	}

	for {
		if gs.playbackGeneration.Load() != generation {
			return
		}

		gs.mu.Lock()
		if gs.playbackGeneration.Load() != generation {
			gs.mu.Unlock()
			return
		}
		if len(gs.songQueue) == 0 {
			seed := gs.lastPlayed
			voiceChannelID := gs.lastVoiceChannelID
			canAutoplay := gs.autoplayEnabled && trackVideoID(seed) != "" && voiceChannelID != ""
			if !canAutoplay {
				gs.isPlaying = false
				controls := gs.clearNowPlayingControlsLocked()
				gs.mu.Unlock()
				clearNowPlayingMessageComponents(s, controls)
				return
			}
			playedVideoIDs := gs.playedVideoIDsLocked()
			gs.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			nextTrack, err := resolveAutoplayTrack(ctx, seed, playedVideoIDs)
			cancel()
			if err != nil {
				log.Printf("autoplay: %v", err)
				if gs.playbackGeneration.Load() != generation {
					return
				}
				_, _ = s.ChannelMessageSend(textChannelID, "Autoplay could not find another related track.")
				if gs.stopAfterAutoplayError(generation) {
					return
				}
				continue
			}

			gs.mu.Lock()
			if gs.playbackGeneration.Load() != generation {
				gs.mu.Unlock()
				return
			}
			if len(gs.songQueue) > 0 {
				gs.mu.Unlock()
				continue
			}
			if !gs.autoplayEnabled {
				gs.isPlaying = false
				gs.mu.Unlock()
				return
			}
			gs.songQueue = append(gs.songQueue, Song{
				guildID:   guildID,
				channelID: voiceChannelID,
				track:     nextTrack,
			})
			gs.mu.Unlock()
			continue
		}
		song := gs.songQueue[0]
		gs.songQueue = gs.songQueue[1:]
		gs.lastPlayed = song.track
		gs.lastVoiceChannelID = song.channelID
		gs.rememberPlayedTrackLocked(song.track)
		if gs.disconnectTimer != nil {
			gs.disconnectTimer.Stop()
			gs.disconnectTimer = nil
		}
		gs.mu.Unlock()

		gs.skipInterrupt.Store(false)

		streamID := streamLogSeq.Add(1)
		trackLabel := trackLogLabel(song.track)
		activeSlots, slotCapacity := streamSlotStats()
		if activeSlots >= slotCapacity {
			log.Printf("stream wait: id=%d guild=%s voice=%s active=%d capacity=%d track=%q url=%s",
				streamID, guildID, song.channelID, activeSlots, slotCapacity, trackLabel, song.track.URL)
		}

		releaseStreamSlot, slotWait, ok := acquireStreamSlot(gs, generation)
		if !ok {
			reason := streamEndReason(nil, gs.skipInterrupt.Load(), gs.playbackGeneration.Load() != generation)
			activeSlots, slotCapacity = streamSlotStats()
			log.Printf("stream wait canceled: id=%d guild=%s reason=%s wait=%s active=%d capacity=%d track=%q url=%s",
				streamID, guildID, reason, slotWait.Round(time.Millisecond), activeSlots, slotCapacity, trackLabel, song.track.URL)
			if gs.playbackGeneration.Load() != generation {
				return
			}
			if gs.skipInterrupt.Load() {
				gs.skipInterrupt.Store(false)
				_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
				continue
			}
			return
		}
		activeSlots, slotCapacity = streamSlotStats()

		vc, err := s.ChannelVoiceJoin(song.guildID, song.channelID, false, true)
		if err != nil {
			releaseStreamSlot()
			activeSlots, slotCapacity = streamSlotStats()
			log.Printf("voice join failed: id=%d guild=%s voice=%s active=%d capacity=%d err=%v",
				streamID, guildID, song.channelID, activeSlots, slotCapacity, err)
			if gs.playbackGeneration.Load() != generation {
				return
			}
			// Put the song back and stop — a transient failure shouldn't silently drop it.
			gs.mu.Lock()
			if gs.playbackGeneration.Load() == generation {
				gs.songQueue = append([]Song{song}, gs.songQueue...)
				gs.isPlaying = false
			}
			gs.mu.Unlock()
			_, _ = s.ChannelMessageSend(textChannelID, fmt.Sprintf("Could not join voice: %v", err))
			return
		}
		log.Printf("voice join succeeded: id=%d guild=%s voice=%s %s",
			streamID, guildID, song.channelID, voiceConnectionSendState(vc))

		sendNowPlayingEmbed(s, gs, textChannelID, song.track)

		streamStarted := time.Now()
		stopMemLogger := startPlaybackMemLogger(streamID, guildID, streamStarted)
		err = streamAudio(gs, vc, dl, song.track.URL, generation)
		interrupted := gs.skipInterrupt.Load()
		generationChanged := gs.playbackGeneration.Load() != generation
		stopMemLogger()
		releaseStreamSlot()
		activeSlots, slotCapacity = streamSlotStats()
		reason := streamEndReason(err, interrupted, generationChanged)
		if err != nil {
			log.Printf("stream audio end: id=%d guild=%s reason=%s duration=%s active=%d capacity=%d %s err=%v",
				streamID, guildID, reason, time.Since(streamStarted).Round(time.Millisecond), activeSlots, slotCapacity, voiceConnectionSendState(vc), err)
		}
		if err != nil {
			if gs.playbackGeneration.Load() != generation {
				return
			}
			if gs.skipInterrupt.Load() {
				gs.skipInterrupt.Store(false)
				_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
			} else {
				if errors.Is(err, errOpusSendTimeout) {
					log.Printf("voice local reset after send timeout: id=%d guild=%s voice=%s %s",
						streamID, guildID, song.channelID, voiceConnectionSendState(vc))
					closeLocalVoiceConnection(s, guildID, vc)
				}
				msg := "Playback error; skipping to next track."
				if gs.disableAutoplayAfterPlaybackError(generation) {
					msg = "Playback error; autoplay disabled to avoid a retry loop. Skipping to next track."
				}
				_, _ = s.ChannelMessageSend(textChannelID, msg)
			}
		} else if gs.skipInterrupt.Load() {
			if gs.playbackGeneration.Load() != generation {
				return
			}
			gs.skipInterrupt.Store(false)
			_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
		}

		// Reset the idle-disconnect timer after each track finishes.
		vconn := vc
		gs.mu.Lock()
		if gs.playbackGeneration.Load() == generation {
			if gs.disconnectTimer != nil {
				gs.disconnectTimer.Stop()
			}
			gs.disconnectTimer = time.AfterFunc(idleDisconnectTimeout, func() {
				safeRun("idle disconnect", func() {
					gs.resetPlaybackState()
					gs.intentionalLeave.Store(true)
					if err := disconnectVoiceConnection(vconn); err != nil {
						log.Printf("idle disconnect: %v", err)
					}
				})
			})
		}
		gs.mu.Unlock()
	}
}

// sendNowPlayingEmbed posts a "Now Playing" embed to the text channel.
// If the track has no metadata (direct URL, no search), only the URL is shown.
func sendNowPlayingEmbed(s *discordgo.Session, gs *guildState, textChannelID string, t Track) {
	controlID, previous := gs.activateNowPlayingControls(textChannelID)
	clearNowPlayingMessageComponents(s, previous)

	msg, err := s.ChannelMessageSendComplex(textChannelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{buildNowPlayingEmbed(t)},
		Components: buildNowPlayingComponents(controlID),
	})
	if err != nil {
		log.Printf("now playing send: %v", err)
		controls := gs.clearNowPlayingControlsFor(controlID)
		clearNowPlayingMessageComponents(s, controls)
		return
	}
	gs.confirmNowPlayingMessage(controlID, msg.ID)
}

func clearNowPlayingMessageComponents(s *discordgo.Session, ref nowPlayingMessageRef) {
	if ref.channelID == "" || ref.messageID == "" {
		return
	}
	components := []discordgo.MessageComponent{}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID:         ref.messageID,
		Channel:    ref.channelID,
		Components: &components,
	}); err != nil {
		log.Printf("clear now playing controls: %v", err)
	}
}

func buildNowPlayingEmbed(t Track) *discordgo.MessageEmbed {
	description := t.URL
	if t.Title != "" {
		description = "**" + t.Title + "**"
		if t.Artist != "" {
			description += "\n" + t.Artist
		}
		if t.Duration != "" {
			description += "\nDuration: " + t.Duration
		}
	}
	return &discordgo.MessageEmbed{
		Title:       "🎵 Now Playing",
		Description: description,
		URL:         t.URL,
	}
}

func buildNowPlayingComponents(controlID string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "Skip",
					Style:    discordgo.PrimaryButton,
					CustomID: nowPlayingCustomID(nowPlayingButtonSkip, controlID),
				},
				discordgo.Button{
					Label:    "Stop",
					Style:    discordgo.DangerButton,
					CustomID: nowPlayingCustomID(nowPlayingButtonStop, controlID),
				},
				discordgo.Button{
					Label:    "Queue",
					Style:    discordgo.SecondaryButton,
					CustomID: nowPlayingCustomID(nowPlayingButtonQueue, controlID),
				},
				discordgo.Button{
					Label:    "Autoplay",
					Style:    discordgo.SecondaryButton,
					CustomID: nowPlayingCustomID(nowPlayingButtonAutoplay, controlID),
				},
			},
		},
	}
}

func nowPlayingCustomID(action, controlID string) string {
	return action + ":" + controlID
}

type queueSnapshot struct {
	current         Track
	hasCurrent      bool
	queued          []Song
	autoplayEnabled bool
	isPlaying       bool
}

func (gs *guildState) snapshotQueue() queueSnapshot {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	queued := make([]Song, len(gs.songQueue))
	copy(queued, gs.songQueue)
	snapshot := queueSnapshot{
		queued:          queued,
		autoplayEnabled: gs.autoplayEnabled,
		isPlaying:       gs.isPlaying,
	}
	if gs.isPlaying && trackHasDisplay(gs.lastPlayed) {
		snapshot.current = gs.lastPlayed
		snapshot.hasCurrent = true
	}
	return snapshot
}

func (gs *guildState) removeQueuedTrack(index int) (Track, int, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	total := len(gs.songQueue)
	if index < 1 || index > total {
		return Track{}, total, false
	}
	removed := gs.songQueue[index-1].track
	gs.songQueue = append(gs.songQueue[:index-1], gs.songQueue[index:]...)
	return removed, total, true
}

func (gs *guildState) moveQueuedTrack(from, to int) (Track, int, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	total := len(gs.songQueue)
	if from < 1 || from > total || to < 1 || to > total {
		return Track{}, total, false
	}
	if from == to {
		return gs.songQueue[from-1].track, total, true
	}
	song := gs.songQueue[from-1]
	without := make([]Song, 0, total-1)
	without = append(without, gs.songQueue[:from-1]...)
	without = append(without, gs.songQueue[from:]...)
	insertAt := to - 1
	reordered := make([]Song, 0, total)
	reordered = append(reordered, without[:insertAt]...)
	reordered = append(reordered, song)
	reordered = append(reordered, without[insertAt:]...)
	gs.songQueue = reordered
	return song.track, total, true
}

func (gs *guildState) clearQueuedTracks() int {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	n := len(gs.songQueue)
	gs.songQueue = nil
	return n
}

func buildQueueEmbed(gs *guildState, page int) *discordgo.MessageEmbed {
	snapshot := gs.snapshotQueue()
	if page < 1 {
		page = 1
	}
	totalQueued := len(snapshot.queued)
	totalPages := 1
	if totalQueued > 0 {
		totalPages = (totalQueued + queuePageSize - 1) / queuePageSize
	}
	if page > totalPages {
		page = totalPages
	}

	status := "Idle"
	if snapshot.isPlaying {
		status = "Playing"
	}
	autoplay := "Off"
	if snapshot.autoplayEnabled {
		autoplay = "On"
	}

	nowPlaying := "Nothing right now."
	if snapshot.hasCurrent {
		nowPlaying = formatQueueTrack(snapshot.current)
	}

	upNext := "Queue is empty."
	if totalQueued > 0 {
		start := (page - 1) * queuePageSize
		end := start + queuePageSize
		if end > totalQueued {
			end = totalQueued
		}
		lines := make([]string, 0, end-start)
		for i, song := range snapshot.queued[start:end] {
			lines = append(lines, fmt.Sprintf("%d. %s", start+i+1, formatQueueTrack(song.track)))
		}
		upNext = truncateFieldValue(strings.Join(lines, "\n"))
	}

	return &discordgo.MessageEmbed{
		Title: "Queue",
		Description: fmt.Sprintf(
			"Status: %s\nAutoplay: %s\nQueued: %d",
			status,
			autoplay,
			totalQueued,
		),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Now Playing",
				Value:  truncateFieldValue(nowPlaying),
				Inline: false,
			},
			{
				Name:   "Up Next",
				Value:  upNext,
				Inline: false,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Page %d/%d", page, totalPages),
		},
	}
}

func trackHasDisplay(t Track) bool {
	return strings.TrimSpace(t.Title) != "" || strings.TrimSpace(t.URL) != ""
}

func formatQueueTrack(t Track) string {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = strings.TrimSpace(t.URL)
	}
	if title == "" {
		title = "Unknown track"
	}
	title = truncateRunes(title, queueLineMaxLen)
	parts := []string{title}
	if artist := strings.TrimSpace(t.Artist); artist != "" {
		parts = append(parts, "- "+truncateRunes(artist, queueLineMaxLen))
	}
	if duration := strings.TrimSpace(t.Duration); duration != "" {
		parts = append(parts, "["+duration+"]")
	}
	return truncateRunes(strings.Join(parts, " "), queueLineMaxLen)
}

func truncateFieldValue(s string) string {
	return truncateRunes(s, discordFieldValueMax)
}

func truncateRunes(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if max <= 0 || len(runes) <= max {
		return string(runes)
	}
	if max == 1 {
		return string(runes[:1])
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
