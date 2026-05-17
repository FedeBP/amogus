package main

// Discord music bot – streams audio from YouTube via yt-dlp + ffmpeg.
//
// Audio pipeline (zero PCM conversion):
//
//	yt-dlp stdout ──pipe──▶ ffmpeg (-c:a libopus, OGG muxer) stdout
//	                                       │
//	                              oggDemux (this file)
//	                                       │
//	                              raw Opus packets ──▶ vc.OpusSend
//
// Removing the PCM → gopus encode step cuts CPU usage significantly because
// ffmpeg's native libopus encoder runs in a single optimised C call rather
// than bouncing through a Go ↔ Cgo ↔ libopus round-trip per 20 ms frame.

import (
	"bytes"
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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type configStruct struct {
	Token     string `json:"token"`
	BotPrefix string `json:"botPrefix"`
	APIKey    string `json:"APIKey"`
}

var (
	Token     string
	BotPrefix string
	APIKey    string
	config    *configStruct
	BotID     string
)

// GetConfig loads configuration from config.json and then overrides any
// fields with environment variables when present.
// Order of precedence: env var > config.json > built-in default.
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

	if BotPrefix == "" {
		BotPrefix = "&"
	}
	if Token == "" {
		log.Fatal("Missing Discord token: set DISCORD_BOT_TOKEN or 'token' in config.json")
	}
	if APIKey == "" {
		log.Fatal("Missing YouTube API key: set YOUTUBE_API_KEY or 'APIKey' in config.json")
	}

	config = &configStruct{Token: Token, BotPrefix: BotPrefix, APIKey: APIKey}
	log.Printf("Configuration loaded.")
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

// ---------------------------------------------------------------------------
// Per-guild playback state
// ---------------------------------------------------------------------------

// guildState holds all mutable playback state for one Discord server.
// Keeping state per-guild ensures that multiple servers never interfere.
type guildState struct {
	mu                 sync.Mutex
	songQueue          []Song
	isPlaying          bool
	autoplayEnabled    bool
	lastPlayed         Track
	lastVoiceChannelID string
	recentVideoIDs     []string
	recentTracks       []Track
	disconnectTimer    *time.Timer
	activeDlCmd        *exec.Cmd
	activeFfCmd        *exec.Cmd
	// skipInterrupt is set to true by /skip to signal streamAudio to halt.
	skipInterrupt      atomic.Bool
	playbackGeneration atomic.Uint64
	// intentionalLeave prevents botVoiceStateHandler from treating a
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

type autoplaySkipRequest struct {
	seed           Track
	voiceChannelID string
	excluded       map[string]struct{}
	history        []Track
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
			excluded:       gs.autoplayExclusionsLocked(),
			history:        gs.autoplayHistoryLocked(),
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

// resetPlaybackState clears the queue, kills active processes, and cancels
// any pending idle-disconnect timer. Used by /stop and external kicks.
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
	gs.recentVideoIDs = nil
	gs.recentTracks = nil
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
	gs := &guildState{}
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
	errPlaylistMaxSize = errors.New("playlist max size reached")

	queuePageMinValue = 1.0
)

const (
	maxPlaylistTracks      = 200
	discordChoiceNameMax   = 100
	autoplayCandidateLimit = 50
	recentVideoMemory      = 50
	recentTrackMemory      = 20
	queuePageSize          = 10
	queueLineMaxLen        = 120
	discordFieldValueMax   = 1024
	idleDisconnectTimeout  = 5 * time.Minute
	opusSendTimeout        = 2 * time.Second
	opusSendPollInterval   = 20 * time.Millisecond
)

const (
	nowPlayingButtonSkip     = "nowplaying:skip"
	nowPlayingButtonStop     = "nowplaying:stop"
	nowPlayingButtonQueue    = "nowplaying:queue"
	nowPlayingButtonAutoplay = "nowplaying:autoplay"
)

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

	session.AddHandler(botVoiceStateHandler)
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

// botVoiceStateHandler detects when the bot is removed from a voice channel
// by an external actor and clears the guild queue accordingly.
// It ignores disconnects that the bot itself initiates (intentionalLeave flag).
func botVoiceStateHandler(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil || vs.UserID != BotID || BotID == "" {
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

func trackHasAutoplayIdentity(t Track) bool {
	return trackVideoID(t) != "" || normalizedAutoplayTitle(t.Title) != "" || normalizedAutoplayArtist(t.Artist) != ""
}

func sameAutoplayIdentity(a, b Track) bool {
	aID, bID := trackVideoID(a), trackVideoID(b)
	if aID != "" && bID != "" {
		return aID == bID
	}
	aTitle, bTitle := normalizedAutoplayTitle(a.Title), normalizedAutoplayTitle(b.Title)
	aArtist, bArtist := normalizedAutoplayArtist(a.Artist), normalizedAutoplayArtist(b.Artist)
	return aTitle != "" && aTitle == bTitle && aArtist != "" && aArtist == bArtist
}

// rememberTrackLocked records recently played tracks so autoplay does not
// immediately loop the same few recommendations. The caller must hold gs.mu.
func (gs *guildState) rememberTrackLocked(t Track) {
	id := trackVideoID(t)
	if id != "" {
		if len(gs.recentVideoIDs) == 0 || gs.recentVideoIDs[len(gs.recentVideoIDs)-1] != id {
			gs.recentVideoIDs = append(gs.recentVideoIDs, id)
			if len(gs.recentVideoIDs) > recentVideoMemory {
				gs.recentVideoIDs = gs.recentVideoIDs[len(gs.recentVideoIDs)-recentVideoMemory:]
			}
		}
	}
	if !trackHasAutoplayIdentity(t) {
		return
	}
	if len(gs.recentTracks) > 0 && sameAutoplayIdentity(gs.recentTracks[len(gs.recentTracks)-1], t) {
		return
	}
	gs.recentTracks = append(gs.recentTracks, t)
	if len(gs.recentTracks) > recentTrackMemory {
		gs.recentTracks = gs.recentTracks[len(gs.recentTracks)-recentTrackMemory:]
	}
}

// autoplayExclusionsLocked returns video IDs already played recently or queued.
// The caller must hold gs.mu.
func (gs *guildState) autoplayExclusionsLocked() map[string]struct{} {
	excluded := make(map[string]struct{}, len(gs.recentVideoIDs)+len(gs.songQueue)+1)
	for _, id := range gs.recentVideoIDs {
		if id != "" {
			excluded[id] = struct{}{}
		}
	}
	if id := trackVideoID(gs.lastPlayed); id != "" {
		excluded[id] = struct{}{}
	}
	for _, song := range gs.songQueue {
		if id := trackVideoID(song.track); id != "" {
			excluded[id] = struct{}{}
		}
	}
	return excluded
}

func (gs *guildState) autoplayHistoryLocked() []Track {
	history := make([]Track, 0, len(gs.recentTracks)+len(gs.songQueue)+1)
	history = append(history, gs.recentTracks...)
	if trackHasAutoplayIdentity(gs.lastPlayed) {
		history = append(history, gs.lastPlayed)
	}
	for _, song := range gs.songQueue {
		if trackHasAutoplayIdentity(song.track) {
			history = append(history, song.track)
		}
	}
	return history
}

func normalizedAutoplayArtist(s string) string {
	return strings.Join(autoplayTextTokens(s, false), " ")
}

func normalizedAutoplayTitle(s string) string {
	return strings.Join(autoplayTextTokens(s, true), " ")
}

func autoplayTextTokens(s string, dropNoise bool) []string {
	s = strings.ToLower(stripBracketedText(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}
	raw := strings.Fields(b.String())
	tokens := make([]string, 0, len(raw))
	for _, token := range raw {
		if dropNoise && isAutoplayTitleNoise(token) {
			continue
		}
		tokens = append(tokens, token)
	}
	return tokens
}

func stripBracketedText(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func isAutoplayTitleNoise(token string) bool {
	switch token {
	case "official", "audio", "video", "music", "lyrics", "lyric", "visualizer",
		"remaster", "remastered", "live", "explicit", "clean", "hd", "hq",
		"feat", "ft", "featuring", "version", "edit", "single", "album":
		return true
	default:
		return false
	}
}

func autoplayTitleSimilar(a, b string) bool {
	aTokens := strings.Fields(normalizedAutoplayTitle(a))
	bTokens := strings.Fields(normalizedAutoplayTitle(b))
	if len(aTokens) == 0 || len(bTokens) == 0 {
		return false
	}
	common := 0
	seen := make(map[string]struct{}, len(aTokens))
	for _, token := range aTokens {
		seen[token] = struct{}{}
	}
	for _, token := range bTokens {
		if _, ok := seen[token]; ok {
			common++
		}
	}
	minLen, maxLen := len(aTokens), len(bTokens)
	if minLen > maxLen {
		minLen, maxLen = maxLen, minLen
	}
	if common == minLen && minLen >= 2 {
		return true
	}
	return common >= 2 && float64(common)/float64(maxLen) >= 0.67
}

func autoplayNearDuplicate(candidate Track, history []Track) bool {
	candidateArtist := normalizedAutoplayArtist(candidate.Artist)
	for _, previous := range history {
		if sameAutoplayIdentity(candidate, previous) {
			return true
		}
		previousArtist := normalizedAutoplayArtist(previous.Artist)
		sameKnownArtist := candidateArtist != "" && candidateArtist == previousArtist
		if sameKnownArtist && autoplayTitleSimilar(candidate.Title, previous.Title) {
			return true
		}
		if candidateArtist == "" && previousArtist == "" && autoplayTitleSimilar(candidate.Title, previous.Title) {
			return true
		}
	}
	return false
}

func autoplayCandidateScore(candidate Track, history []Track, position int) int {
	score := 1000 - position
	candidateArtist := normalizedAutoplayArtist(candidate.Artist)
	for i := len(history) - 1; i >= 0; i-- {
		previous := history[i]
		recencyPenalty := len(history) - i
		previousArtist := normalizedAutoplayArtist(previous.Artist)
		if candidateArtist != "" && candidateArtist == previousArtist {
			score -= 15
			if recencyPenalty <= 3 {
				score -= 25
			}
		}
		if autoplayTitleSimilar(candidate.Title, previous.Title) {
			score -= 50
			if recencyPenalty <= 5 {
				score -= 80
			}
		}
	}
	return score
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
	for _, r := range results {
		if r.VideoID == "" {
			continue
		}
		trackMetadataCache[r.VideoID] = cachedTrackMetadata{
			track:   trackFromSearchResult(r),
			expires: expires,
		}
	}
}

func cachedTrackByVideoID(videoID string) (Track, bool) {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()

	cached, ok := trackMetadataCache[videoID]
	if !ok {
		return Track{}, false
	}
	if time.Now().After(cached.expires) {
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
	searchCacheMu.Lock()
	if cached, ok := searchCache[query]; ok && time.Now().Before(cached.expires) {
		searchCacheMu.Unlock()
		return cached.results, nil
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

	expires := time.Now().Add(10 * time.Minute)
	searchCacheMu.Lock()
	searchCache[query] = cachedSearch{
		results: results,
		expires: expires,
	}
	cacheTrackMetadataLocked(results, expires)
	searchCacheMu.Unlock()

	return results, nil
}

// ytMusicRadio asks the Python sidecar for YouTube Music radio suggestions
// based on a seed video ID.
func ytMusicRadio(ctx context.Context, videoID string, limit int) ([]SearchResult, error) {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, errors.New("empty seed video ID")
	}

	params := url.Values{}
	params.Set("videoId", videoID)
	params.Set("limit", fmt.Sprint(limit))

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
	cacheTrackMetadataLocked(results, time.Now().Add(10*time.Minute))
	searchCacheMu.Unlock()
	return results, nil
}

func chooseAutoplayTrack(results []SearchResult, excluded map[string]struct{}, history []Track) (Track, error) {
	var best Track
	bestScore := -1 << 30

	for i, r := range results {
		if r.VideoID == "" {
			continue
		}
		if _, seen := excluded[r.VideoID]; seen {
			continue
		}
		track := trackFromSearchResult(r)
		score := autoplayCandidateScore(track, history, i)
		if autoplayNearDuplicate(track, history) {
			continue
		}
		if score > bestScore {
			best = track
			bestScore = score
		}
	}
	if best.URL != "" {
		return best, nil
	}
	return Track{}, errors.New("no fresh radio suggestions found")
}

func resolveAutoplayTrack(ctx context.Context, seed Track, excluded map[string]struct{}, history []Track) (Track, error) {
	seedID := trackVideoID(seed)
	if seedID == "" {
		return Track{}, errors.New("last track has no YouTube video ID")
	}

	results, err := ytMusicRadio(ctx, seedID, autoplayCandidateLimit)
	if err != nil {
		return Track{}, err
	}
	return chooseAutoplayTrack(results, excluded, history)
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

// runPlayFlow is the top-level handler for a /play command.
// It decides whether the query is a playlist or a single track, then
// delegates to the appropriate enqueue function.  Returns a human-readable
// status string suitable for sending back to the user (may be empty for the
// single-track happy path, where the Now Playing embed speaks for itself).
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

// playNextSong is the main playback loop for a guild.  It runs as a goroutine
// and processes the queue until it is empty, then returns.
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
				gs.mu.Unlock()
				return
			}
			excluded := gs.autoplayExclusionsLocked()
			history := gs.autoplayHistoryLocked()
			gs.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			nextTrack, err := resolveAutoplayTrack(ctx, seed, excluded, history)
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
		gs.rememberTrackLocked(song.track)
		if gs.disconnectTimer != nil {
			gs.disconnectTimer.Stop()
			gs.disconnectTimer = nil
		}
		gs.mu.Unlock()

		gs.skipInterrupt.Store(false)

		vc, err := s.ChannelVoiceJoin(song.guildID, song.channelID, false, true)
		if err != nil {
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

		sendNowPlayingEmbed(s, textChannelID, song.track)

		err = streamAudio(gs, vc, dl, song.track.URL, generation)
		if err != nil {
			if gs.playbackGeneration.Load() != generation {
				return
			}
			if gs.skipInterrupt.Load() {
				gs.skipInterrupt.Store(false)
				_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
			} else {
				log.Printf("stream: %v", err)
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
func sendNowPlayingEmbed(s *discordgo.Session, textChannelID string, t Track) {
	_, _ = s.ChannelMessageSendComplex(textChannelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{buildNowPlayingEmbed(t)},
		Components: buildNowPlayingComponents(),
	})
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

func buildNowPlayingComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "Skip",
					Style:    discordgo.PrimaryButton,
					CustomID: nowPlayingButtonSkip,
				},
				discordgo.Button{
					Label:    "Stop",
					Style:    discordgo.DangerButton,
					CustomID: nowPlayingButtonStop,
				},
				discordgo.Button{
					Label:    "Queue",
					Style:    discordgo.SecondaryButton,
					CustomID: nowPlayingButtonQueue,
				},
				discordgo.Button{
					Label:    "Autoplay",
					Style:    discordgo.SecondaryButton,
					CustomID: nowPlayingButtonAutoplay,
				},
			},
		},
	}
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
// ffmpeg writes an OGG container when told to encode with libopus.  OGG pages
// have a fixed 27-byte header plus a variable-length segment table.  The first
// two pages are the Opus identification and comment headers; every subsequent
// page carries audio packets that can be forwarded verbatim to Discord.
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

// readOggPage reads and parses the next OGG page from r.
func readOggPage(r io.Reader) (*oggPage, error) {
	// Fixed 27-byte header.
	var header [27]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	if string(header[0:4]) != oggCapturePattern {
		return nil, fmt.Errorf("ogg: bad capture pattern %q", header[0:4])
	}

	page := &oggPage{
		headerType:  header[5],
		granulePos:  binary.LittleEndian.Uint64(header[6:14]),
		sequenceNum: binary.LittleEndian.Uint32(header[18:22]),
	}

	nSegs := int(header[26])
	segTable := make([]byte, nSegs)
	if _, err := io.ReadFull(r, segTable); err != nil {
		return nil, fmt.Errorf("ogg: reading segment table: %w", err)
	}

	// Read the page body as one contiguous slice, then split into packets
	// using the lacing values.  A segment of 255 bytes means the packet
	// continues in the next segment; anything shorter terminates it.
	bodyLen := 0
	for _, s := range segTable {
		bodyLen += int(s)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("ogg: reading page body: %w", err)
	}

	offset := 0
	var pkt []byte
	for _, s := range segTable {
		pkt = append(pkt, body[offset:offset+int(s)]...)
		offset += int(s)
		if s < 255 { // last segment of this packet
			page.packets = append(page.packets, pkt)
			pkt = nil
		}
	}
	if len(pkt) > 0 {
		// Continued packet with no terminator on this page (rare).
		page.packets = append(page.packets, pkt)
	}

	return page, nil
}

// ---------------------------------------------------------------------------
// Audio streaming
// ---------------------------------------------------------------------------

func processOutputTail(name string, stderr *bytes.Buffer) string {
	output := strings.TrimSpace(stderr.String())
	if output == "" {
		return ""
	}
	const maxOutputLen = 2000
	if len(output) > maxOutputLen {
		output = output[len(output)-maxOutputLen:]
	}
	return name + " stderr: " + output
}

func streamProcessError(reason string, err error, dlStderr, ffStderr *bytes.Buffer) error {
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

func sendOpusPacket(gs *guildState, vc *discordgo.VoiceConnection, pkt []byte, generation uint64) error {
	if vc == nil || vc.OpusSend == nil {
		return errors.New("voice connection is not ready to send audio")
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
			return errors.New("timed out sending audio packet to voice connection")
		case <-ticker.C:
		}
	}
}

// streamAudio pipes yt-dlp into ffmpeg (native Opus encoding) and forwards
// raw Opus packets directly to Discord — no PCM conversion or Go-side encoding.
//
// Pipeline:
//
//	yt-dlp -f bestaudio -o - <url>
//	      └─pipe─▶ ffmpeg -i pipe:0 -c:a libopus -ar 48000 -ac 2
//	                       -b:a 128k -vbr on -f ogg pipe:1
//	                              └─oggDemux─▶ vc.OpusSend
//
// The two Opus header pages (identification + comment) are skipped;
// every audio packet thereafter is sent as-is.
func streamAudio(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string, generation uint64) error {
	var dlStderr, ffStderr bytes.Buffer

	dlCmd := exec.Command(dl,
		"--no-playlist",
		"-f", "bestaudio",
		"-o", "-",
		youtubeURL,
	)
	dlCmd.Stderr = &dlStderr

	ffCmd := exec.Command("ffmpeg",
		"-i", "pipe:0",
		"-c:a", "libopus",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", "128k",
		"-vbr", "on",
		"-f", "ogg",
		"pipe:1",
	)
	ffCmd.Stderr = &ffStderr

	dlStdout, err := dlCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("yt-dlp stdout pipe: %w", err)
	}
	ffCmd.Stdin = dlStdout

	ffStdout, err := ffCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	if err := dlCmd.Start(); err != nil {
		return streamProcessError("yt-dlp start", err, &dlStderr, &ffStderr)
	}
	if err := ffCmd.Start(); err != nil {
		_ = dlCmd.Process.Kill()
		return streamProcessError("ffmpeg start", err, &dlStderr, &ffStderr)
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

	// Skip the two mandatory Opus header pages (identification + comment).
	// pageIndex 0 = OpusHead, pageIndex 1 = OpusTags.
	for skip := 0; skip < 2; skip++ {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			break
		}
		if _, err := readOggPage(ffStdout); err != nil {
			if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return streamProcessError(fmt.Sprintf("ogg header page %d", skip), err, &dlStderr, &ffStderr)
		}
	}

	// Stream audio pages until EOF or skip signal.
	for {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			break
		}

		page, err := readOggPage(ffStdout)
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
			return streamProcessError("ogg demux", err, &dlStderr, &ffStderr)
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
					return err
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
		return nil
	}
	if dlErr != nil {
		return streamProcessError("yt-dlp exit", dlErr, &dlStderr, &ffStderr)
	}
	if ffErr != nil {
		return streamProcessError("ffmpeg exit", ffErr, &dlStderr, &ffStderr)
	}
	return nil
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

func respondQueueEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState, page int) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{buildQueueEmbed(gs, page)},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})
}

func stopPlayback(s *discordgo.Session, guildID string, gs *guildState) {
	gs.resetPlaybackState()
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

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		nextTrack, err := resolveAutoplayTrack(ctx, req.seed, req.excluded, req.history)
		cancel()
		if err != nil {
			log.Printf("skip autoplay: %v", err)
			_, _ = s.ChannelMessageSend(textChannelID, "Autoplay could not find another related track.")
			return
		}
		_ = enqueueTrack(s, guildID, textChannelID, req.voiceChannelID, nextTrack)
	}()

	return "Skipped. Finding another related track..."
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
		switch i.ApplicationCommandData().Name {
		case "play":
			handleSlashPlay(s, i)
		case "skip":
			handleSlashSkip(s, i, gs)
		case "stop":
			stopPlayback(s, i.GuildID, gs)
			_ = respondEphemeral(s, i, "Stopped and cleared the queue.")
		case "shuffle":
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
			_ = respondEphemeral(s, i, "Queue shuffled.")
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
		}

	case discordgo.InteractionMessageComponent:
		if i.GuildID == "" {
			_ = respondEphemeral(s, i, "Use this bot inside a server.")
			return
		}
		handleNowPlayingButton(s, i, getGuildState(i.GuildID))
	}
}

func handleNowPlayingButton(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	switch i.MessageComponentData().CustomID {
	case nowPlayingButtonSkip:
		_ = respondEphemeral(s, i, skipPlayback(s, i.GuildID, i.ChannelID, gs))
	case nowPlayingButtonStop:
		stopPlayback(s, i.GuildID, gs)
		_ = respondEphemeral(s, i, "Stopped and cleared the queue.")
	case nowPlayingButtonQueue:
		_ = respondQueueEmbed(s, i, gs, 1)
	case nowPlayingButtonAutoplay:
		enabled := setAutoplay(gs, nil)
		_ = respondEphemeral(s, i, autoplayStatusMessage(enabled))
	default:
		_ = respondEphemeral(s, i, "That button is no longer available.")
	}
}

func handleSlashSkip(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	_ = respondEphemeral(s, i, skipPlayback(s, i.GuildID, i.ChannelID, gs))
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
		_ = respondEphemeral(s, i, fmt.Sprintf("There is no queued track #%d. Queue size: %d.", index, total))
		return
	}
	_ = respondEphemeral(s, i, fmt.Sprintf("Removed #%d: %s", index, formatQueueTrack(removed)))
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
		_ = respondEphemeral(s, i, fmt.Sprintf("Could not move #%d to #%d. Queue size: %d.", from, to, total))
		return
	}
	if from == to {
		_ = respondEphemeral(s, i, fmt.Sprintf("#%d is already there: %s", from, formatQueueTrack(moved)))
		return
	}
	_ = respondEphemeral(s, i, fmt.Sprintf("Moved #%d to #%d: %s", from, to, formatQueueTrack(moved)))
}

func handleSlashClear(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	n := gs.clearQueuedTracks()
	if n == 0 {
		_ = respondEphemeral(s, i, "Queue is already empty.")
		return
	}
	_ = respondEphemeral(s, i, fmt.Sprintf("Cleared %d queued %s.", n, pluralize(n, "track", "tracks")))
}

func handleSlashQueue(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	page := 1
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "page" {
			page = int(opt.IntValue())
			break
		}
	}
	_ = respondQueueEmbed(s, i, gs, page)
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
	_ = respondEphemeral(s, i, autoplayStatusMessage(enabled))
}

// handleSlashPlay handles the /play command: verifies the user is in a voice
// channel, defers the response immediately (to avoid Discord's 3-second
// timeout), then resolves and enqueues the track in a goroutine.
func handleSlashPlay(s *discordgo.Session, i *discordgo.InteractionCreate) {
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

	// Defer immediately so Discord does not time out during search/resolve.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		log.Printf("slash defer: %v", err)
		return
	}

	ch, guildID := i.ChannelID, i.GuildID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		msg := runPlayFlow(ctx, s, guildID, ch, voiceChID, query)
		if msg == "" {
			msg = "Queued — starting playback in your voice channel."
		}
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); err != nil {
			log.Printf("slash edit: %v", err)
			_, _ = s.ChannelMessageSend(ch, msg)
		}
	}()
}
