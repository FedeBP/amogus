package main

// Discord music bot – streams audio from YouTube via yt-dlp + ffmpeg.
//
// Audio pipeline:
//
//	yt-dlp stdout -> ffmpeg OGG remux/copy or libopus encode -> oggReader
//	                                                       -> vc.OpusSend
//
// The preferred path asks yt-dlp for an existing Opus stream and lets ffmpeg
// remux it into OGG without re-encoding. Tracks without an Opus format fall
// back to ffmpeg's native libopus encoder.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type configStruct struct {
	Token                string `json:"token"`
	BotPrefix            string `json:"botPrefix"`
	APIKey               string `json:"APIKey"`
	MaxConcurrentStreams int    `json:"MAX_CONCURRENT_STREAMS"`
}

var (
	Token     string
	BotPrefix string
	APIKey    string
	config    *configStruct
	BotID     string
)

// GetConfig loads file-backed settings, applies environment overrides, and
// reads env-only runtime diagnostics.
// Order of precedence for shared settings: env var > config.json > default.
func GetConfig() {
	log.Printf("Reading configuration...")

	var fileCfg configStruct
	if data, err := os.ReadFile("./config.json"); err == nil {
		if err := json.Unmarshal(data, &fileCfg); err != nil {
			log.Fatalf("config.json parse error: %v", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("config.json read error: %v", err)
	}

	Token = strings.TrimSpace(fileCfg.Token)
	BotPrefix = strings.TrimSpace(fileCfg.BotPrefix)
	APIKey = strings.TrimSpace(fileCfg.APIKey)

	if v := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN")); v != "" {
		Token = v
	}
	if v := strings.TrimSpace(os.Getenv("YOUTUBE_API_KEY")); v != "" {
		APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("BOT_PREFIX")); v != "" {
		BotPrefix = v
	}
	maxStreams := fileCfg.MaxConcurrentStreams
	if v := strings.TrimSpace(os.Getenv("MAX_CONCURRENT_STREAMS")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			log.Fatalf("MAX_CONCURRENT_STREAMS must be a positive integer, got %q", v)
		}
		maxStreams = parsed
	}
	memLogEvery, err := parsePlaybackMemLogInterval(os.Getenv("PLAYBACK_MEM_LOG_INTERVAL"))
	if err != nil {
		log.Fatalf("PLAYBACK_MEM_LOG_INTERVAL must be a Go duration like 30s or a positive number of seconds, got %q", os.Getenv("PLAYBACK_MEM_LOG_INTERVAL"))
	}

	if BotPrefix == "" {
		BotPrefix = "&"
	}
	if Token == "" {
		log.Fatal("Missing Discord token: set DISCORD_BOT_TOKEN or 'token' in config.json")
	}
	if APIKey == "" {
		log.Fatal("Missing YouTube API key: set YOUTUBE_API_KEY or 'APIKey' in config.json")
	}

	configureStreamLimiter(maxStreams)
	playbackMemLogEvery = memLogEvery

	config = &configStruct{
		Token:                Token,
		BotPrefix:            BotPrefix,
		APIKey:               APIKey,
		MaxConcurrentStreams: maxConcurrentStreams,
	}
	log.Printf("Configuration loaded. Max concurrent streams: %d.", maxConcurrentStreams)
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Track is metadata for a single playable item.
type Track struct {
	URL      string
	Title    string
	Artist   string
	Duration string
}

// Song pairs a Track with the Discord IDs needed to play it.
type Song struct {
	guildID   string
	channelID string
	track     Track
}

// SearchResult is the JSON shape returned by the Python search sidecar.
type SearchResult struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	VideoID  string `json:"videoId"`
	Duration string `json:"duration"`
}

type guildAction func()

type audioPipeline struct {
	name        string
	ytdlpFormat string
	ffmpegArgs  []string
}

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

func disconnectVoiceConnection(vc *discordgo.VoiceConnection) error {
	if vc == nil {
		return nil
	}
	// discordgo may attempt to reconnect a VoiceConnection using its last
	// ChannelID after transport errors. Clear it before closing local voice
	// resources so an external kick or stop cannot auto-rejoin later.
	vc.Lock()
	vc.ChannelID = ""
	vc.Unlock()
	return vc.Disconnect()
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
	go gs.runActions()
	return gs
}

func (gs *guildState) runActions() {
	for action := range gs.actionQueue {
		action()
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

// ---------------------------------------------------------------------------
// Global helpers
// ---------------------------------------------------------------------------

var (
	rngMu sync.Mutex
	rng   = rand.New(rand.NewSource(time.Now().UnixNano()))

	errNotInVoice      = errors.New("not in a voice channel")
	errOpusSendTimeout = errors.New("timed out sending audio packet to voice connection")
	errPlaylistMaxSize = errors.New("playlist max size reached")

	queuePageMinValue = 1.0
)

const (
	maxPlaylistTracks     = 200
	discordChoiceNameMax  = 100
	queuePageSize         = 10
	queueLineMaxLen       = 120
	discordFieldValueMax  = 1024
	guildActionQueueSize  = 64
	defaultMaxStreams     = 5
	searchCacheMaxEntries = 256
	trackCacheMaxEntries  = 2048
	searchCacheTTL        = 10 * time.Minute
	idleDisconnectTimeout = 5 * time.Minute
	opusSendTimeout       = 2 * time.Second
	opusSendPollInterval  = 20 * time.Millisecond
	processOutputTailMax  = 2000
)

const (
	nowPlayingButtonSkip     = "nowplaying:skip"
	nowPlayingButtonStop     = "nowplaying:stop"
	nowPlayingButtonQueue    = "nowplaying:queue"
	nowPlayingButtonAutoplay = "nowplaying:autoplay"
)

var (
	opusCopyPipeline = audioPipeline{
		name:        "opus_copy",
		ytdlpFormat: "bestaudio[acodec=opus]",
		ffmpegArgs: []string{
			"-nostdin",
			"-hide_banner",
			"-loglevel", "warning",
			"-i", "pipe:0",
			"-c:a", "copy",
			"-f", "ogg",
			"pipe:1",
		},
	}
	opusTranscodePipeline = audioPipeline{
		name:        "opus_transcode",
		ytdlpFormat: "bestaudio",
		ffmpegArgs: []string{
			"-nostdin",
			"-hide_banner",
			"-loglevel", "warning",
			"-i", "pipe:0",
			"-c:a", "libopus",
			"-ar", "48000",
			"-ac", "2",
			"-b:a", "128k",
			"-vbr", "on",
			"-f", "ogg",
			"pipe:1",
		},
	}
)

var (
	streamSlots          = make(chan struct{}, defaultMaxStreams)
	maxConcurrentStreams = defaultMaxStreams
	streamSlotsMu        sync.Mutex
	streamLogSeq         atomic.Uint64
	playbackMemLogEvery  time.Duration
)

func configureStreamLimiter(limit int) {
	if limit < 1 {
		limit = defaultMaxStreams
	}
	streamSlotsMu.Lock()
	defer streamSlotsMu.Unlock()
	streamSlots = make(chan struct{}, limit)
	maxConcurrentStreams = limit
}

func streamSlotChannel() chan struct{} {
	streamSlotsMu.Lock()
	defer streamSlotsMu.Unlock()
	return streamSlots
}

func streamSlotStats() (int, int) {
	slots := streamSlotChannel()
	return len(slots), cap(slots)
}

func parsePlaybackMemLogInterval(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 1 {
			return 0, fmt.Errorf("interval must be positive")
		}
		return time.Duration(seconds) * time.Second, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if interval <= 0 {
		return 0, fmt.Errorf("interval must be positive")
	}
	return interval, nil
}

func acquireStreamSlot(gs *guildState, generation uint64) (func(), time.Duration, bool) {
	slots := streamSlotChannel()
	waitStarted := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			return nil, time.Since(waitStarted), false
		}
		select {
		case slots <- struct{}{}:
			return func() { <-slots }, time.Since(waitStarted), true
		case <-ticker.C:
		}
	}
}

func trackLogLabel(t Track) string {
	label := strings.TrimSpace(t.Title)
	if t.Artist != "" {
		if label != "" {
			label += " - "
		}
		label += strings.TrimSpace(t.Artist)
	}
	if label == "" {
		label = t.URL
	}
	return truncateRunes(label, 120)
}

func streamEndReason(err error, interrupted, generationChanged bool) string {
	switch {
	case generationChanged:
		return "generation_changed"
	case interrupted:
		return "interrupted"
	case err != nil:
		return "error"
	default:
		return "finished"
	}
}

type playbackMemSnapshot struct {
	heapAlloc    uint64
	heapSys      uint64
	heapIdle     uint64
	heapReleased uint64
	stackInuse   uint64
	nextGC       uint64
	sys          uint64
	rss          uint64
	hasRSS       bool
	numGC        uint32
	goroutines   int
}

func capturePlaybackMemSnapshot() playbackMemSnapshot {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	rss, hasRSS := readProcessRSSBytes()
	return playbackMemSnapshot{
		heapAlloc:    m.HeapAlloc,
		heapSys:      m.HeapSys,
		heapIdle:     m.HeapIdle,
		heapReleased: m.HeapReleased,
		stackInuse:   m.StackInuse,
		nextGC:       m.NextGC,
		sys:          m.Sys,
		rss:          rss,
		hasRSS:       hasRSS,
		numGC:        m.NumGC,
		goroutines:   runtime.NumGoroutine(),
	}
}

func readProcessRSSBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}

func logPlaybackMemSnapshot(event string, streamID uint64, guildID string, elapsed time.Duration) {
	s := capturePlaybackMemSnapshot()
	active, capacity := streamSlotStats()
	fields := fmt.Sprintf(
		"playback mem: event=%s id=%d guild=%s elapsed=%s active=%d capacity=%d goroutines=%d heap_alloc=%d heap_sys=%d heap_idle=%d heap_released=%d stack_inuse=%d next_gc=%d sys=%d num_gc=%d",
		event,
		streamID,
		guildID,
		elapsed.Round(time.Millisecond),
		active,
		capacity,
		s.goroutines,
		s.heapAlloc,
		s.heapSys,
		s.heapIdle,
		s.heapReleased,
		s.stackInuse,
		s.nextGC,
		s.sys,
		s.numGC,
	)
	if s.hasRSS {
		fields += fmt.Sprintf(" rss=%d", s.rss)
	}
	log.Print(fields)
}

func startPlaybackMemLogger(streamID uint64, guildID string, started time.Time) func() {
	interval := playbackMemLogEvery
	if interval <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once

	logPlaybackMemSnapshot("start", streamID, guildID, time.Since(started))
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logPlaybackMemSnapshot("tick", streamID, guildID, time.Since(started))
			case <-stop:
				return
			}
		}
	}()

	return func() {
		once.Do(func() {
			close(stop)
			<-done
			logPlaybackMemSnapshot("end", streamID, guildID, time.Since(started))
		})
	}
}

// searchCache caches YTMusic search results for 10 minutes to avoid
// redundant HTTP round-trips to the Python sidecar during autocomplete.
var (
	searchCacheMu      sync.Mutex
	searchCache        = map[string]cachedSearch{}
	trackMetadataCache = map[string]cachedTrackMetadata{}
)

type cachedSearch struct {
	results []SearchResult
	expires time.Time
}

type cachedTrackMetadata struct {
	track   Track
	expires time.Time
}

// searchHTTP is a shared HTTP client for the Python search sidecar.
// Using a single client reuses TCP connections across requests.
var searchHTTP = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ---------------------------------------------------------------------------
// Slash command definitions
// ---------------------------------------------------------------------------

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "play",
		Description: "Play music from YouTube Music",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:         discordgo.ApplicationCommandOptionString,
				Name:         "query",
				Description:  "Search terms, or paste a watch / playlist URL",
				Required:     true,
				Autocomplete: true,
			},
		},
	},
	{Name: "skip", Description: "Skip the current track"},
	{Name: "stop", Description: "Stop playback and clear the queue"},
	{Name: "shuffle", Description: "Shuffle the queue"},
	{
		Name:        "remove",
		Description: "Remove a track from the queue",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "index",
				Description: "Queue number to remove",
				Required:    true,
				MinValue:    &queuePageMinValue,
			},
		},
	},
	{
		Name:        "move",
		Description: "Move a queued track to another position",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "from",
				Description: "Queue number to move",
				Required:    true,
				MinValue:    &queuePageMinValue,
			},
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "to",
				Description: "New queue position",
				Required:    true,
				MinValue:    &queuePageMinValue,
			},
		},
	},
	{Name: "clear", Description: "Clear queued tracks without stopping playback"},
	{
		Name:        "queue",
		Description: "Show what is playing and what is queued",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "page",
				Description: "Queue page to show",
				Required:    false,
				MinValue:    &queuePageMinValue,
			},
		},
	},
	{
		Name:        "autoplay",
		Description: "Toggle related-track autoplay when the queue ends",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "enabled",
				Description: "Set autoplay on or off. Omit to toggle.",
				Required:    false,
			},
		},
	},
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	GetConfig()
	Start()
	<-make(chan struct{}) // block forever; bot runs on goroutines/handlers
}

// Start initialises the Discord session, registers event handlers,
// and opens the WebSocket gateway connection.
func Start() {
	session, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Couldn't initialise bot: %v", err)
	}

	user, err := session.User("@me")
	if err != nil {
		log.Fatalf("Error getting bot user: %v", err)
	}
	BotID = user.ID

	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildVoiceStates

	session.AddHandler(voiceStateHandler)
	session.AddHandler(interactionHandler)
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		// Overwrite all slash commands on startup so any definition changes
		// take effect without manual intervention.
		appID := r.User.ID
		if r.Application != nil && r.Application.ID != "" {
			appID = r.Application.ID
		}
		if _, err := s.ApplicationCommandBulkOverwrite(appID, "", slashCommands); err != nil {
			log.Printf("register slash commands: %v", err)
		} else {
			log.Printf("Slash commands registered.")
		}
	})

	if err = session.Open(); err != nil {
		log.Fatalf("Error opening session: %v", err)
	}
	log.Printf("Bot initialised successfully.")
}

// ---------------------------------------------------------------------------
// Voice helpers
// ---------------------------------------------------------------------------

// voiceChannelForUser returns the voice channel ID that userID is currently
// in within guildID, or errNotInVoice if they are not in any channel.
func voiceChannelForUser(s *discordgo.Session, guildID, userID string) (string, error) {
	g, err := s.State.Guild(guildID)
	if err != nil {
		return "", err
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID == userID && vs.ChannelID != "" {
			return vs.ChannelID, nil
		}
	}
	return "", errNotInVoice
}

// voiceStateHandler clears playback when the bot leaves voice or when its
// current voice channel becomes empty.
func voiceStateHandler(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil || BotID == "" {
		return
	}
	if vs.UserID != BotID {
		maybeDisconnectEmptyVoiceChannel(s, vs.GuildID)
		return
	}
	if vs.ChannelID != "" {
		return // bot joined or moved — not a leave event
	}

	gs := getGuildState(vs.GuildID)
	wasIntentional := gs.intentionalLeave.CompareAndSwap(true, false)
	if wasIntentional {
		log.Printf("Bot left voice in guild %s; clearing playback state.", vs.GuildID)
	} else {
		log.Printf("Bot was removed from voice in guild %s; clearing playback state.", vs.GuildID)
	}

	gs.resetPlaybackState()

	s.RLock()
	vc, ok := s.VoiceConnections[vs.GuildID]
	s.RUnlock()
	if ok && vc != nil {
		if err := disconnectVoiceConnection(vc); err != nil {
			log.Printf("voice cleanup after disconnect: %v", err)
		}
	}
}

func maybeDisconnectEmptyVoiceChannel(s *discordgo.Session, guildID string) {
	channelID := botVoiceChannelID(s, guildID)
	if channelID == "" || !voiceChannelIsEmpty(s, guildID, channelID) {
		return
	}

	gs := getGuildState(guildID)
	if !gs.enqueueAction(func() {
		channelID := botVoiceChannelID(s, guildID)
		if channelID == "" || !voiceChannelIsEmpty(s, guildID, channelID) {
			return
		}
		disconnectEmptyVoiceChannel(s, guildID, gs, channelID)
	}) {
		log.Printf("empty voice disconnect skipped in guild %s: action queue is full", guildID)
	}
}

func botVoiceChannelID(s *discordgo.Session, guildID string) string {
	if s == nil {
		return ""
	}
	s.RLock()
	vc := s.VoiceConnections[guildID]
	s.RUnlock()
	if vc != nil {
		vc.RLock()
		channelID := vc.ChannelID
		vc.RUnlock()
		if channelID != "" {
			return channelID
		}
	}

	if s.State == nil {
		return ""
	}
	g, err := s.State.Guild(guildID)
	if err != nil {
		return ""
	}
	for _, voiceState := range g.VoiceStates {
		if voiceState.UserID == BotID {
			return voiceState.ChannelID
		}
	}
	return ""
}

func voiceChannelIsEmpty(s *discordgo.Session, guildID, channelID string) bool {
	if s == nil || s.State == nil {
		return false
	}
	g, err := s.State.Guild(guildID)
	if err != nil {
		log.Printf("empty voice check: %v", err)
		return false
	}
	return !voiceChannelHasListeners(g, channelID)
}

func voiceChannelHasListeners(g *discordgo.Guild, channelID string) bool {
	if g == nil || channelID == "" {
		return false
	}
	for _, voiceState := range g.VoiceStates {
		if voiceState.ChannelID == channelID && voiceState.UserID != BotID {
			return true
		}
	}
	return false
}

func disconnectEmptyVoiceChannel(s *discordgo.Session, guildID string, gs *guildState, channelID string) {
	log.Printf("Voice channel %s in guild %s is empty; stopping playback.", channelID, guildID)
	controls := gs.clearNowPlayingControlsFor("")
	gs.resetPlaybackState()
	clearNowPlayingMessageComponents(s, controls)

	s.RLock()
	vc := s.VoiceConnections[guildID]
	s.RUnlock()
	if vc == nil {
		return
	}
	vc.RLock()
	currentChannelID := vc.ChannelID
	vc.RUnlock()
	if currentChannelID != "" && currentChannelID != channelID {
		return
	}
	gs.intentionalLeave.Store(true)
	if err := disconnectVoiceConnection(vc); err != nil {
		log.Printf("empty voice disconnect: %v", err)
	}
}

// ---------------------------------------------------------------------------
// URL helpers
// ---------------------------------------------------------------------------

// extractYouTubeVideoID parses a YouTube URL and returns the video ID,
// or an empty string if the input is not a recognised YouTube URL.
// Handles youtube.com/watch, youtu.be short links, and /shorts/ URLs.
func extractYouTubeVideoID(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	switch host {
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		if strings.HasPrefix(u.Path, "/watch") {
			return u.Query().Get("v")
		}
		if strings.HasPrefix(u.Path, "/shorts/") {
			id := strings.TrimPrefix(u.Path, "/shorts/")
			if i := strings.Index(id, "/"); i >= 0 {
				id = id[:i]
			}
			return id
		}
	case "youtu.be":
		id := strings.Trim(strings.TrimPrefix(u.Path, "/"), "/")
		if i := strings.Index(id, "/"); i >= 0 {
			id = id[:i]
		}
		return id
	}
	return ""
}

func trackVideoID(t Track) string {
	return extractYouTubeVideoID(t.URL)
}

func trackFromSearchResult(r SearchResult) Track {
	return Track{
		URL:      "https://www.youtube.com/watch?v=" + r.VideoID,
		Title:    r.Title,
		Artist:   r.Artist,
		Duration: r.Duration,
	}
}

// playedVideoIDsLocked returns a snapshot of tracks played in the current
// voice session. The caller must hold gs.mu.
func (gs *guildState) playedVideoIDsLocked() map[string]struct{} {
	played := make(map[string]struct{}, len(gs.playedVideoIDs))
	for id := range gs.playedVideoIDs {
		played[id] = struct{}{}
	}
	return played
}

// rememberPlayedTrackLocked records an exact video ID for the current session.
// The caller must hold gs.mu.
func (gs *guildState) rememberPlayedTrackLocked(track Track) {
	id := trackVideoID(track)
	if id == "" {
		return
	}
	if gs.playedVideoIDs == nil {
		gs.playedVideoIDs = make(map[string]struct{})
	}
	gs.playedVideoIDs[id] = struct{}{}
}

// ytDownloaderPath returns the absolute path of yt-dlp (preferred) or
// youtube-dl, whichever is found first on PATH.
func ytDownloaderPath() (string, error) {
	for _, name := range []string{"yt-dlp", "youtube-dl"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("yt-dlp not found in PATH")
}

// ---------------------------------------------------------------------------
// Search / track resolution
// ---------------------------------------------------------------------------

func cacheTrackMetadataLocked(results []SearchResult, expires time.Time) {
	now := time.Now()
	pruneExpiredTrackMetadataLocked(now)
	for _, r := range results {
		if r.VideoID == "" {
			continue
		}
		trackMetadataCache[r.VideoID] = cachedTrackMetadata{
			track:   trackFromSearchResult(r),
			expires: expires,
		}
	}
	trimTrackMetadataCacheLocked(trackCacheMaxEntries)
}

func cachedSearchResultsLocked(query string, now time.Time) ([]SearchResult, bool) {
	cached, ok := searchCache[query]
	if !ok {
		return nil, false
	}
	if !now.Before(cached.expires) {
		delete(searchCache, query)
		return nil, false
	}
	return cached.results, true
}

func cacheSearchResultsLocked(query string, results []SearchResult, expires time.Time) {
	now := time.Now()
	pruneExpiredSearchCacheLocked(now)
	searchCache[query] = cachedSearch{
		results: results,
		expires: expires,
	}
	trimSearchCacheLocked(searchCacheMaxEntries)
}

func pruneExpiredSearchCacheLocked(now time.Time) {
	for query, cached := range searchCache {
		if !now.Before(cached.expires) {
			delete(searchCache, query)
		}
	}
}

func pruneExpiredTrackMetadataLocked(now time.Time) {
	for videoID, cached := range trackMetadataCache {
		if !now.Before(cached.expires) {
			delete(trackMetadataCache, videoID)
		}
	}
}

func trimSearchCacheLocked(maxEntries int) {
	for len(searchCache) > maxEntries {
		var oldestKey string
		var oldestExpires time.Time
		for query, cached := range searchCache {
			if oldestKey == "" || cached.expires.Before(oldestExpires) {
				oldestKey = query
				oldestExpires = cached.expires
			}
		}
		if oldestKey == "" {
			return
		}
		delete(searchCache, oldestKey)
	}
}

func trimTrackMetadataCacheLocked(maxEntries int) {
	for len(trackMetadataCache) > maxEntries {
		var oldestKey string
		var oldestExpires time.Time
		for videoID, cached := range trackMetadataCache {
			if oldestKey == "" || cached.expires.Before(oldestExpires) {
				oldestKey = videoID
				oldestExpires = cached.expires
			}
		}
		if oldestKey == "" {
			return
		}
		delete(trackMetadataCache, oldestKey)
	}
}

func cachedTrackByVideoID(videoID string) (Track, bool) {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()

	now := time.Now()
	pruneExpiredTrackMetadataLocked(now)
	cached, ok := trackMetadataCache[videoID]
	if !ok {
		return Track{}, false
	}
	if !now.Before(cached.expires) {
		delete(trackMetadataCache, videoID)
		return Track{}, false
	}
	return cached.track, true
}

// resolveTrack turns a raw query string into a playable Track.
//
//   - If query is a YouTube URL, the video ID is extracted directly.
//   - Otherwise the Python YTMusic sidecar is queried and the first
//     suitable result is returned.
func resolveTrack(ctx context.Context, query string) (Track, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Track{}, errors.New("empty query")
	}

	if id := extractYouTubeVideoID(query); id != "" {
		if track, ok := cachedTrackByVideoID(id); ok {
			return track, nil
		}
		return Track{URL: "https://www.youtube.com/watch?v=" + id}, nil
	}

	results, err := ytMusicSearch(ctx, query)
	if err != nil {
		return Track{}, err
	}
	if len(results) == 0 {
		return Track{}, errors.New("no suitable results found")
	}

	r := results[0]
	return trackFromSearchResult(r), nil
}

// ytMusicSearch queries the Python search sidecar for up to 5 song results.
// Results are cached for 10 minutes, so rapid autocomplete keystrokes do not
// hammer the sidecar with duplicate requests.
func ytMusicSearch(ctx context.Context, query string) ([]SearchResult, error) {
	now := time.Now()
	searchCacheMu.Lock()
	if cached, ok := cachedSearchResultsLocked(query, now); ok {
		searchCacheMu.Unlock()
		return cached, nil
	}
	searchCacheMu.Unlock()

	reqURL := "http://127.0.0.1:5000/search?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("search sidecar status %s", resp.Status)
	}

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	expires := time.Now().Add(searchCacheTTL)
	searchCacheMu.Lock()
	cacheSearchResultsLocked(query, results, expires)
	cacheTrackMetadataLocked(results, expires)
	searchCacheMu.Unlock()

	return results, nil
}

// ytMusicRadio asks the Python sidecar for YouTube Music radio suggestions
// based on a seed video ID.
func ytMusicRadio(ctx context.Context, videoID string) ([]SearchResult, error) {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, errors.New("empty seed video ID")
	}

	params := url.Values{}
	params.Set("videoId", videoID)

	reqURL := "http://127.0.0.1:5000/radio?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("radio sidecar status %s", resp.Status)
	}

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	searchCacheMu.Lock()
	cacheTrackMetadataLocked(results, time.Now().Add(searchCacheTTL))
	searchCacheMu.Unlock()
	return results, nil
}

func chooseAutoplayTrack(results []SearchResult, seedID string, playedVideoIDs map[string]struct{}) (Track, error) {
	for _, r := range results {
		if r.VideoID == "" || r.VideoID == seedID {
			continue
		}
		if _, played := playedVideoIDs[r.VideoID]; played {
			continue
		}
		return trackFromSearchResult(r), nil
	}
	return Track{}, errors.New("no radio suggestions found")
}

func resolveAutoplayTrack(ctx context.Context, seed Track, playedVideoIDs map[string]struct{}) (Track, error) {
	seedID := trackVideoID(seed)
	if seedID == "" {
		return Track{}, errors.New("last track has no YouTube video ID")
	}

	results, err := ytMusicRadio(ctx, seedID)
	if err != nil {
		return Track{}, err
	}
	return chooseAutoplayTrack(results, seedID, playedVideoIDs)
}

// youtubeAutocompleteChoices returns Discord autocomplete choices for the
// given partial query string. Returns an empty slice (never nil) on error
// so Discord always receives a valid (possibly empty) autocomplete response.
func youtubeAutocompleteChoices(ctx context.Context, query string) ([]*discordgo.ApplicationCommandOptionChoice, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []*discordgo.ApplicationCommandOptionChoice{}, nil
	}

	results, err := ytMusicSearch(ctx, query)
	if err != nil {
		return nil, err
	}

	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(results))
	for _, r := range results {
		if r.VideoID == "" {
			continue
		}
		label := r.Title
		if r.Artist != "" {
			label += " · " + r.Artist
		}
		if r.Duration != "" {
			label += " [" + r.Duration + "]"
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  truncateChoiceLabel(label, discordChoiceNameMax),
			Value: "https://www.youtube.com/watch?v=" + r.VideoID,
		})
	}
	return choices, nil
}

// truncateChoiceLabel shortens s to at most max runes, appending "…" if cut.
func truncateChoiceLabel(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}

// ---------------------------------------------------------------------------
// Playlist support
// ---------------------------------------------------------------------------

// fetchPlaylistEnqueue resolves a YouTube playlist URL and enqueues each
// video using the YouTube Data API v3.  It pages through results in batches
// of 50, capped at maxPlaylistTracks, so playback can start before the full
// list has been fetched.
func fetchPlaylistEnqueue(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, playlistURL string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(playlistURL))
	if err != nil {
		return 0, fmt.Errorf("invalid URL: %w", err)
	}
	playlistID := strings.TrimSpace(u.Query().Get("list"))
	if playlistID == "" {
		return 0, errors.New("no list= parameter in URL — paste the full playlist URL")
	}

	svc, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return 0, fmt.Errorf("youtube client: %w", err)
	}

	total := 0
	call := svc.PlaylistItems.List([]string{"contentDetails"}).
		PlaylistId(playlistID).MaxResults(50).Context(ctx)

	err = call.Pages(ctx, func(page *youtube.PlaylistItemListResponse) error {
		for _, item := range page.Items {
			if item.ContentDetails == nil || item.ContentDetails.VideoId == "" {
				continue
			}
			if total >= maxPlaylistTracks {
				return errPlaylistMaxSize
			}
			_ = enqueueTrack(s, guildID, textChannelID, voiceChannelID,
				Track{URL: "https://www.youtube.com/watch?v=" + item.ContentDetails.VideoId})
			total++
		}
		return nil
	})

	if errors.Is(err, errPlaylistMaxSize) {
		return total, errPlaylistMaxSize
	}
	return total, err
}

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
		go playNextSong(s, guildID, textChannelID, generation)
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
		log.Printf("stream slot acquired: id=%d guild=%s voice=%s wait=%s active=%d capacity=%d track=%q url=%s",
			streamID, guildID, song.channelID, slotWait.Round(time.Millisecond), activeSlots, slotCapacity, trackLabel, song.track.URL)

		log.Printf("voice join start: id=%d guild=%s voice=%s", streamID, guildID, song.channelID)
		vc, err := s.ChannelVoiceJoin(song.guildID, song.channelID, false, true)
		if err != nil {
			releaseStreamSlot()
			activeSlots, slotCapacity = streamSlotStats()
			log.Printf("voice join failed: id=%d guild=%s voice=%s active=%d capacity=%d err=%v",
				streamID, guildID, song.channelID, activeSlots, slotCapacity, err)
			if gs.playbackGeneration.Load() != generation {
				return
			}
			log.Printf("voice join: %v", err)
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
		log.Printf("voice join ok: id=%d guild=%s voice=%s", streamID, guildID, song.channelID)

		sendNowPlayingEmbed(s, gs, textChannelID, song.track)

		streamStarted := time.Now()
		log.Printf("stream audio start: id=%d guild=%s voice=%s track=%q url=%s",
			streamID, guildID, song.channelID, trackLabel, song.track.URL)
		stopMemLogger := startPlaybackMemLogger(streamID, guildID, streamStarted)
		err = streamAudio(gs, vc, dl, song.track.URL, generation)
		interrupted := gs.skipInterrupt.Load()
		generationChanged := gs.playbackGeneration.Load() != generation
		stopMemLogger()
		releaseStreamSlot()
		activeSlots, slotCapacity = streamSlotStats()
		reason := streamEndReason(err, interrupted, generationChanged)
		if err != nil {
			log.Printf("stream audio end: id=%d guild=%s reason=%s duration=%s active=%d capacity=%d err=%v",
				streamID, guildID, reason, time.Since(streamStarted).Round(time.Millisecond), activeSlots, slotCapacity, err)
		} else {
			log.Printf("stream audio end: id=%d guild=%s reason=%s duration=%s active=%d capacity=%d",
				streamID, guildID, reason, time.Since(streamStarted).Round(time.Millisecond), activeSlots, slotCapacity)
		}
		if err != nil {
			if gs.playbackGeneration.Load() != generation {
				return
			}
			if gs.skipInterrupt.Load() {
				gs.skipInterrupt.Store(false)
				_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
			} else {
				log.Printf("stream: %v", err)
				if errors.Is(err, errOpusSendTimeout) {
					log.Printf("voice local reset after send timeout: id=%d guild=%s voice=%s",
						streamID, guildID, song.channelID)
					vc.Close()
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
				gs.resetPlaybackState()
				gs.intentionalLeave.Store(true)
				if err := disconnectVoiceConnection(vconn); err != nil {
					log.Printf("idle disconnect: %v", err)
				}
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

// ---------------------------------------------------------------------------
// OGG/Opus demuxer
// ---------------------------------------------------------------------------
//
// ffmpeg writes an OGG container for both the Opus-copy and libopus-transcode
// pipelines. OGG pages have a fixed 27-byte header plus a variable-length
// segment table. The first two pages are the Opus identification and comment
// headers; subsequent pages carry Opus packets that are forwarded to Discord.
//
// OGG page layout (https://www.rfc-editor.org/rfc/rfc3533):
//
//	Bytes  0– 3  capture pattern "OggS"
//	Byte   4     version (always 0)
//	Byte   5     header type flags
//	Bytes  6–13  granule position (uint64 LE)
//	Bytes 14–17  bitstream serial number (uint32 LE)
//	Bytes 18–21  page sequence number (uint32 LE)
//	Bytes 22–25  CRC checksum (uint32 LE)
//	Byte  26     number of segments (N)
//	Bytes 27–26+N  segment table (N bytes, each = segment length, 255 = continuation)
//	Remaining   page body (sum of segment lengths)

const oggCapturePattern = "OggS"

// oggPage is one decoded OGG page.
type oggPage struct {
	sequenceNum uint32
	granulePos  uint64
	headerType  byte
	packets     [][]byte // reassembled lacing packets on this page
}

type oggReader struct {
	header      [27]byte
	segTable    [255]byte
	packetSizes [255]int
	packets     [255][]byte
	packetSlots int
	page        oggPage
}

// readOggPage reads and parses the next OGG page from r.
func readOggPage(r io.Reader) (*oggPage, error) {
	var reader oggReader
	return reader.readPage(r)
}

// readPage reads and parses the next OGG page from r, reusing parser scratch
// buffers across calls. Packet byte slices are still newly allocated because
// vc.OpusSend consumes them asynchronously.
func (or *oggReader) readPage(r io.Reader) (*oggPage, error) {
	// Fixed 27-byte header.
	header := or.header[:]
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 'O' || header[1] != 'g' || header[2] != 'g' || header[3] != 'S' {
		return nil, fmt.Errorf("ogg: bad capture pattern %q", header[0:4])
	}

	page := &or.page
	page.headerType = header[5]
	page.granulePos = binary.LittleEndian.Uint64(header[6:14])
	page.sequenceNum = binary.LittleEndian.Uint32(header[18:22])
	for i := 0; i < or.packetSlots; i++ {
		or.packets[i] = nil
	}
	page.packets = or.packets[:0]
	or.packetSlots = 0

	nSegs := int(header[26])
	segTable := or.segTable[:nSegs]
	if _, err := io.ReadFull(r, segTable); err != nil {
		return nil, fmt.Errorf("ogg: reading segment table: %w", err)
	}

	// The lacing table tells us each packet's size before we read the page body.
	// Reading packet-sized chunks avoids allocating a whole-page body and then
	// copying each packet out of it.
	packetCount := 0
	packetLen := 0
	for _, s := range segTable {
		packetLen += int(s)
		if s < 255 { // last segment of this packet
			or.packetSizes[packetCount] = packetLen
			packetCount++
			packetLen = 0
		}
	}
	if packetLen > 0 {
		// Continued packet with no terminator on this page (rare).
		or.packetSizes[packetCount] = packetLen
		packetCount++
	}

	for i := 0; i < packetCount; i++ {
		pkt := make([]byte, or.packetSizes[i])
		if _, err := io.ReadFull(r, pkt); err != nil {
			return nil, fmt.Errorf("ogg: reading page body: %w", err)
		}
		page.packets = append(page.packets, pkt)
	}
	or.packetSlots = packetCount

	return page, nil
}

// ---------------------------------------------------------------------------
// Audio streaming
// ---------------------------------------------------------------------------

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer {
	if max < 1 {
		max = processOutputTailMax
	}
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(p) >= b.max {
		if cap(b.buf) < b.max {
			b.buf = make([]byte, b.max)
		}
		b.buf = b.buf[:b.max]
		copy(b.buf, p[len(p)-b.max:])
		return len(p), nil
	}

	b.buf = append(b.buf, p...)
	if overflow := len(b.buf) - b.max; overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:b.max]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func processOutputTail(name string, stderr *tailBuffer) string {
	if stderr == nil {
		return ""
	}
	output := strings.TrimSpace(stderr.String())
	if output == "" {
		return ""
	}
	return name + " stderr: " + output
}

func streamProcessError(reason string, err error, dlStderr, ffStderr *tailBuffer) error {
	details := make([]string, 0, 2)
	if output := processOutputTail("yt-dlp", dlStderr); output != "" {
		details = append(details, output)
	}
	if output := processOutputTail("ffmpeg", ffStderr); output != "" {
		details = append(details, output)
	}
	if len(details) == 0 {
		return fmt.Errorf("%s: %w", reason, err)
	}
	return fmt.Errorf("%s: %w; %s", reason, err, strings.Join(details, " | "))
}

func voiceConnectionSendState(vc *discordgo.VoiceConnection) string {
	if vc == nil {
		return "voice_state=nil"
	}
	vc.RLock()
	ready := vc.Ready
	guildID := vc.GuildID
	channelID := vc.ChannelID
	vc.RUnlock()

	queueLen, queueCap := 0, 0
	if vc.OpusSend != nil {
		queueLen = len(vc.OpusSend)
		queueCap = cap(vc.OpusSend)
	}
	return fmt.Sprintf("voice_state=ready:%t guild:%s channel:%s opus_queue:%d/%d",
		ready, guildID, channelID, queueLen, queueCap)
}

func sendOpusPacket(gs *guildState, vc *discordgo.VoiceConnection, pkt []byte, generation uint64) error {
	if vc == nil || vc.OpusSend == nil {
		return errors.New("voice connection is not ready to send audio")
	}
	if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
		return nil
	}
	select {
	case vc.OpusSend <- pkt:
		return nil
	default:
	}

	timeout := time.NewTimer(opusSendTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(opusSendPollInterval)
	defer ticker.Stop()
	for {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			return nil
		}
		select {
		case vc.OpusSend <- pkt:
			return nil
		case <-timeout.C:
			return fmt.Errorf("%w (%s)", errOpusSendTimeout, voiceConnectionSendState(vc))
		case <-ticker.C:
		}
	}
}

// streamAudio pipes yt-dlp into ffmpeg and forwards Opus packets to Discord.
// It prefers remuxing an existing YouTube Opus stream and falls back to libopus
// transcoding only if the copy pipeline fails before sending audio packets.
func streamAudio(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string, generation uint64) error {
	packetsSent, err := streamAudioPipeline(gs, vc, dl, youtubeURL, generation, opusCopyPipeline)
	if err == nil || gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
		return err
	}
	if packetsSent > 0 {
		return err
	}
	log.Printf("stream pipeline fallback: from=%s to=%s url=%s err=%v",
		opusCopyPipeline.name, opusTranscodePipeline.name, youtubeURL, err)
	_, err = streamAudioPipeline(gs, vc, dl, youtubeURL, generation, opusTranscodePipeline)
	return err
}

func ytdlpCommandArgs(pipeline audioPipeline, youtubeURL string) []string {
	return []string{
		"--no-playlist",
		"--no-progress",
		"--no-update",
		"--remote-components", "ejs:github",
		// Avoid YouTube clients that intermittently return HTTP 403 media URLs.
		"--extractor-args", "youtube:player_client=default,web_embedded,-android_vr,-android_sdkless;player_js_version=actual",
		"-f", pipeline.ytdlpFormat,
		"-o", "-",
		youtubeURL,
	}
}

func streamAudioPipeline(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string, generation uint64, pipeline audioPipeline) (int, error) {
	dlStderr := newTailBuffer(processOutputTailMax)
	ffStderr := newTailBuffer(processOutputTailMax)

	dlCmd := exec.Command(dl, ytdlpCommandArgs(pipeline, youtubeURL)...)
	dlCmd.Stderr = dlStderr

	ffCmd := exec.Command("ffmpeg", pipeline.ffmpegArgs...)
	ffCmd.Stderr = ffStderr

	dlStdout, err := dlCmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("%s yt-dlp stdout pipe: %w", pipeline.name, err)
	}
	ffCmd.Stdin = dlStdout

	ffStdout, err := ffCmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("%s ffmpeg stdout pipe: %w", pipeline.name, err)
	}

	if err := dlCmd.Start(); err != nil {
		return 0, streamProcessError(pipeline.name+" yt-dlp start", err, dlStderr, ffStderr)
	}
	if err := ffCmd.Start(); err != nil {
		_ = dlCmd.Process.Kill()
		return 0, streamProcessError(pipeline.name+" ffmpeg start", err, dlStderr, ffStderr)
	}

	// Register active processes so skip/stop can kill them immediately.
	gs.mu.Lock()
	gs.activeDlCmd = dlCmd
	gs.activeFfCmd = ffCmd
	gs.mu.Unlock()

	defer func() {
		gs.mu.Lock()
		if gs.activeDlCmd == dlCmd {
			gs.activeDlCmd = nil
		}
		if gs.activeFfCmd == ffCmd {
			gs.activeFfCmd = nil
		}
		gs.mu.Unlock()
	}()

	var ogg oggReader

	// Skip the two mandatory Opus header pages (identification + comment).
	// pageIndex 0 = OpusHead, pageIndex 1 = OpusTags.
	for skip := 0; skip < 2; skip++ {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			break
		}
		if _, err := ogg.readPage(ffStdout); err != nil {
			if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return 0, streamProcessError(fmt.Sprintf("%s ogg header page %d", pipeline.name, skip), err, dlStderr, ffStderr)
		}
	}

	// Stream audio pages until EOF, skip, or playback generation change.
	packetsSent := 0
	for {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			break
		}

		page, err := ogg.readPage(ffStdout)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // normal end of stream
			}
			if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return packetsSent, streamProcessError(pipeline.name+" ogg demux", err, dlStderr, ffStderr)
		}

		for _, pkt := range page.packets {
			if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
				break
			}
			if len(pkt) > 0 {
				if err := sendOpusPacket(gs, vc, pkt, generation); err != nil {
					_ = dlCmd.Process.Kill()
					_ = ffCmd.Process.Kill()
					_, _ = dlCmd.Wait(), ffCmd.Wait()
					return packetsSent, err
				}
				if !gs.skipInterrupt.Load() && gs.playbackGeneration.Load() == generation {
					packetsSent++
				}
			}
		}
	}

	if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
		_ = dlCmd.Process.Kill()
		_ = ffCmd.Process.Kill()
	}

	dlErr := dlCmd.Wait()
	ffErr := ffCmd.Wait()
	if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
		return packetsSent, nil
	}
	if dlErr != nil {
		return packetsSent, streamProcessError(pipeline.name+" yt-dlp exit", dlErr, dlStderr, ffStderr)
	}
	if ffErr != nil {
		return packetsSent, streamProcessError(pipeline.name+" ffmpeg exit", ffErr, dlStderr, ffStderr)
	}
	return packetsSent, nil
}

// ---------------------------------------------------------------------------
// Interaction handler
// ---------------------------------------------------------------------------

// interactionUserID returns the Discord user ID from an interaction,
// handling both guild (Member) and DM (User) contexts.
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// respondEphemeral sends an ephemeral (user-only visible) text response
// to a slash command interaction.
func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func deferInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) error {
	data := &discordgo.InteractionResponseData{}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: data,
	})
}

func editInteractionContent(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		log.Printf("interaction edit: %v", err)
	}
}

func editInteractionEmbeds(s *discordgo.Session, i *discordgo.InteractionCreate, embeds []*discordgo.MessageEmbed) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &embeds}); err != nil {
		log.Printf("interaction embed edit: %v", err)
	}
}

func stopPlayback(s *discordgo.Session, guildID string, gs *guildState) {
	controls := gs.clearNowPlayingControlsFor("")
	gs.resetPlaybackState()
	clearNowPlayingMessageComponents(s, controls)
	s.RLock()
	vc, ok := s.VoiceConnections[guildID]
	s.RUnlock()
	if ok && vc != nil {
		gs.intentionalLeave.Store(true)
		if err := disconnectVoiceConnection(vc); err != nil {
			log.Printf("stop disconnect: %v", err)
		}
	}
}

func setAutoplay(gs *guildState, requestedState *bool) bool {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	enabled := !gs.autoplayEnabled
	if requestedState != nil {
		enabled = *requestedState
	}
	gs.autoplayEnabled = enabled
	return enabled
}

func autoplayStatusMessage(enabled bool) string {
	if enabled {
		return "Autoplay enabled. When the queue ends, I will add a related track."
	}
	return "Autoplay disabled."
}

func skipPlayback(s *discordgo.Session, guildID, textChannelID string, gs *guildState) string {
	req, shouldAutoplay := gs.prepareSkip()
	if !shouldAutoplay {
		return "Skipped."
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	nextTrack, err := resolveAutoplayTrack(ctx, req.seed, req.playedVideoIDs)
	cancel()
	if err != nil {
		log.Printf("skip autoplay: %v", err)
		return "Skipped. Autoplay could not find another related track."
	}
	_ = enqueueTrack(s, guildID, textChannelID, req.voiceChannelID, nextTrack)
	return "Skipped. Queued another related track."
}

func enqueueDeferredAction(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState, action guildAction) {
	if gs.enqueueAction(action) {
		return
	}
	editInteractionContent(s, i, "This server has too many queued music actions. Try again in a moment.")
}

// interactionHandler dispatches both autocomplete and slash command events.
func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {

	case discordgo.InteractionApplicationCommandAutocomplete:
		data := i.ApplicationCommandData()
		if data.Name != "play" {
			return
		}
		var query string
		for _, opt := range data.Options {
			if opt.Focused && opt.Name == "query" {
				query = opt.StringValue()
				break
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		choices, err := youtubeAutocompleteChoices(ctx, query)
		if err != nil {
			log.Printf("autocomplete: %v", err)
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		}
		if choices == nil {
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: choices},
		})

	case discordgo.InteractionApplicationCommand:
		if i.GuildID == "" {
			_ = respondEphemeral(s, i, "Use this bot inside a server.")
			return
		}
		gs := getGuildState(i.GuildID)
		if i.ApplicationCommandData().Name == "play" {
			handleSlashPlay(s, i, gs)
			return
		}
		if err := deferInteraction(s, i, true); err != nil {
			log.Printf("slash defer: %v", err)
			return
		}
		enqueueDeferredAction(s, i, gs, func() {
			handleQueuedSlashCommand(s, i, gs)
		})

	case discordgo.InteractionMessageComponent:
		if i.GuildID == "" {
			_ = respondEphemeral(s, i, "Use this bot inside a server.")
			return
		}
		gs := getGuildState(i.GuildID)
		if err := deferInteraction(s, i, true); err != nil {
			log.Printf("component defer: %v", err)
			return
		}
		enqueueDeferredAction(s, i, gs, func() {
			handleNowPlayingButton(s, i, gs)
		})
	}
}

func handleQueuedSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	switch i.ApplicationCommandData().Name {
	case "skip":
		handleSlashSkip(s, i, gs)
	case "stop":
		stopPlayback(s, i.GuildID, gs)
		editInteractionContent(s, i, "Stopped and cleared the queue.")
	case "shuffle":
		handleSlashShuffle(s, i, gs)
	case "remove":
		handleSlashRemove(s, i, gs)
	case "move":
		handleSlashMove(s, i, gs)
	case "clear":
		handleSlashClear(s, i, gs)
	case "queue":
		handleSlashQueue(s, i, gs)
	case "autoplay":
		handleSlashAutoplay(s, i, gs)
	default:
		editInteractionContent(s, i, "Unknown command.")
	}
}

func handleNowPlayingButton(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	action, ok := gs.activeNowPlayingAction(i.MessageComponentData().CustomID)
	if !ok {
		editInteractionContent(s, i, "That Now Playing message is no longer active.")
		return
	}
	switch action {
	case nowPlayingButtonSkip:
		editInteractionContent(s, i, skipPlayback(s, i.GuildID, i.ChannelID, gs))
	case nowPlayingButtonStop:
		stopPlayback(s, i.GuildID, gs)
		editInteractionContent(s, i, "Stopped and cleared the queue.")
	case nowPlayingButtonQueue:
		editInteractionEmbeds(s, i, []*discordgo.MessageEmbed{buildQueueEmbed(gs, 1)})
	case nowPlayingButtonAutoplay:
		enabled := setAutoplay(gs, nil)
		editInteractionContent(s, i, autoplayStatusMessage(enabled))
	default:
		editInteractionContent(s, i, "That button is no longer available.")
	}
}

func handleSlashSkip(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	editInteractionContent(s, i, skipPlayback(s, i.GuildID, i.ChannelID, gs))
}

func handleSlashShuffle(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	gs.mu.Lock()
	n := len(gs.songQueue)
	if n > 1 {
		rngMu.Lock()
		rng.Shuffle(n, func(i, j int) {
			gs.songQueue[i], gs.songQueue[j] = gs.songQueue[j], gs.songQueue[i]
		})
		rngMu.Unlock()
	}
	gs.mu.Unlock()
	editInteractionContent(s, i, "Queue shuffled.")
}

func handleSlashRemove(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	index := 0
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "index" {
			index = int(opt.IntValue())
			break
		}
	}
	removed, total, ok := gs.removeQueuedTrack(index)
	if !ok {
		editInteractionContent(s, i, fmt.Sprintf("There is no queued track #%d. Queue size: %d.", index, total))
		return
	}
	editInteractionContent(s, i, fmt.Sprintf("Removed #%d: %s", index, formatQueueTrack(removed)))
}

func handleSlashMove(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	from, to := 0, 0
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "from":
			from = int(opt.IntValue())
		case "to":
			to = int(opt.IntValue())
		}
	}
	moved, total, ok := gs.moveQueuedTrack(from, to)
	if !ok {
		editInteractionContent(s, i, fmt.Sprintf("Could not move #%d to #%d. Queue size: %d.", from, to, total))
		return
	}
	if from == to {
		editInteractionContent(s, i, fmt.Sprintf("#%d is already there: %s", from, formatQueueTrack(moved)))
		return
	}
	editInteractionContent(s, i, fmt.Sprintf("Moved #%d to #%d: %s", from, to, formatQueueTrack(moved)))
}

func handleSlashClear(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	n := gs.clearQueuedTracks()
	if n == 0 {
		editInteractionContent(s, i, "Queue is already empty.")
		return
	}
	editInteractionContent(s, i, fmt.Sprintf("Cleared %d queued %s.", n, pluralize(n, "track", "tracks")))
}

func handleSlashQueue(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	page := 1
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "page" {
			page = int(opt.IntValue())
			break
		}
	}
	editInteractionEmbeds(s, i, []*discordgo.MessageEmbed{buildQueueEmbed(gs, page)})
}

func handleSlashAutoplay(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	var requestedState *bool
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "enabled" {
			value := opt.BoolValue()
			requestedState = &value
			break
		}
	}

	enabled := setAutoplay(gs, requestedState)
	editInteractionContent(s, i, autoplayStatusMessage(enabled))
}

// handleSlashPlay handles the /play command: verifies the user is in a voice
// channel, defers the response immediately (to avoid Discord's 3-second
// timeout), then resolves and enqueues the track through the per-guild action lane.
func handleSlashPlay(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	uid := interactionUserID(i)
	if uid == "" {
		_ = respondEphemeral(s, i, "Could not identify your user.")
		return
	}

	voiceChID, err := voiceChannelForUser(s, i.GuildID, uid)
	if err != nil {
		msg := "Join a voice channel first, then run `/play`."
		if !errors.Is(err, errNotInVoice) {
			msg = "Could not read voice state. Try again in a moment."
		}
		_ = respondEphemeral(s, i, msg)
		return
	}

	var query string
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "query" {
			query = strings.TrimSpace(opt.StringValue())
			break
		}
	}
	if query == "" {
		_ = respondEphemeral(s, i, "Enter a search term or paste a YouTube URL.")
		return
	}

	if err := deferInteraction(s, i, false); err != nil {
		log.Printf("slash defer: %v", err)
		return
	}

	ch, guildID := i.ChannelID, i.GuildID
	enqueueDeferredAction(s, i, gs, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		msg := runPlayFlow(ctx, s, guildID, ch, voiceChID, query)
		if msg == "" {
			msg = "Queued — starting playback in your voice channel."
		}
		editInteractionContent(s, i, msg)
	})
}
