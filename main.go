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

	Token     = strings.TrimSpace(fileCfg.Token)
	BotPrefix = strings.TrimSpace(fileCfg.BotPrefix)
	APIKey    = strings.TrimSpace(fileCfg.APIKey)

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
	mu              sync.Mutex
	songQueue       []Song
	isPlaying       bool
	disconnectTimer *time.Timer
	activeDlCmd     *exec.Cmd
	activeFfCmd     *exec.Cmd
	// skipInterrupt is set to true by skip/stop to signal streamAudio to halt.
	skipInterrupt    atomic.Bool
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

// resetPlaybackState clears the queue, kills active processes, and cancels
// any pending idle-disconnect timer. Used by &stop and external kicks.
func (gs *guildState) resetPlaybackState() {
	gs.killPlaybackProcesses()
	gs.skipInterrupt.Store(false)
	gs.mu.Lock()
	gs.songQueue = nil
	gs.isPlaying = false
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
)

const (
	maxPlaylistTracks    = 200
	discordChoiceNameMax = 100
)

// searchCache caches YTMusic search results for 10 minutes to avoid
// redundant HTTP round-trips to the Python sidecar during autocomplete.
var (
	searchCacheMu sync.Mutex
	searchCache   = map[string]cachedSearch{}
)

type cachedSearch struct {
	results []SearchResult
	expires time.Time
}

// searchHTTP is a shared HTTP client for the Python search sidecar.
// Using a single client reuses TCP connections across requests.
var searchHTTP = &http.Client{
	Timeout: 2 * time.Second,
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
	{Name: "skip",    Description: "Skip the current track"},
	{Name: "stop",    Description: "Stop playback and clear the queue"},
	{Name: "shuffle", Description: "Shuffle the queue"},
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
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildVoiceStates |
		discordgo.IntentsMessageContent

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
	log.Printf("Bot initialised successfully. 👾")
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
	if gs.intentionalLeave.CompareAndSwap(true, false) {
		return // we triggered this disconnect ourselves; ignore it
	}

	log.Printf("Bot was removed from voice in guild %s; clearing queue.", vs.GuildID)
	gs.resetPlaybackState()

	s.RLock()
	vc, ok := s.VoiceConnections[vs.GuildID]
	s.RUnlock()
	if ok && vc != nil {
		gs.intentionalLeave.Store(true)
		_ = vc.Disconnect()
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
	return Track{
		URL:      "https://www.youtube.com/watch?v=" + r.VideoID,
		Title:    r.Title,
		Artist:   r.Artist,
		Duration: r.Duration,
	}, nil
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

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	searchCacheMu.Lock()
	searchCache[query] = cachedSearch{
		results: results,
		expires: time.Now().Add(10 * time.Minute),
	}
	searchCacheMu.Unlock()

	return results, nil
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
	if shouldStart {
		gs.isPlaying = true
	}
	gs.mu.Unlock()

	if shouldStart {
		go playNextSong(s, guildID, textChannelID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Playback loop
// ---------------------------------------------------------------------------

// playNextSong is the main playback loop for a guild.  It runs as a goroutine
// and processes the queue until it is empty, then returns.
func playNextSong(s *discordgo.Session, guildID, textChannelID string) {
	gs := getGuildState(guildID)

	dl, err := ytDownloaderPath()
	if err != nil {
		log.Printf("downloader: %v", err)
		_, _ = s.ChannelMessageSend(textChannelID, "yt-dlp must be installed and on PATH.")
		gs.mu.Lock()
		gs.isPlaying = false
		gs.mu.Unlock()
		return
	}

	for {
		gs.mu.Lock()
		if len(gs.songQueue) == 0 {
			gs.isPlaying = false
			if gs.disconnectTimer != nil {
				gs.disconnectTimer.Stop()
				gs.disconnectTimer = nil
			}
			gs.mu.Unlock()
			return
		}
		song := gs.songQueue[0]
		gs.songQueue = gs.songQueue[1:]
		gs.mu.Unlock()

		gs.skipInterrupt.Store(false)

		vc, err := s.ChannelVoiceJoin(song.guildID, song.channelID, false, true)
		if err != nil {
			log.Printf("voice join: %v", err)
			// Put the song back and stop — a transient failure shouldn't silently drop it.
			gs.mu.Lock()
			gs.songQueue = append([]Song{song}, gs.songQueue...)
			gs.isPlaying = false
			gs.mu.Unlock()
			_, _ = s.ChannelMessageSend(textChannelID, fmt.Sprintf("Could not join voice: %v", err))
			return
		}

		sendNowPlayingEmbed(s, textChannelID, song.track)

		if err := streamAudio(gs, vc, dl, song.track.URL); err != nil {
			if gs.skipInterrupt.Load() {
				gs.skipInterrupt.Store(false)
				_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
			} else {
				log.Printf("stream: %v", err)
				_, _ = s.ChannelMessageSend(textChannelID, "Playback error; skipping to next track.")
			}
		} else if gs.skipInterrupt.Load() {
			gs.skipInterrupt.Store(false)
			_, _ = s.ChannelMessageSend(textChannelID, "Skipped.")
		}

		// Reset the idle-disconnect timer after each track finishes.
		if gs.disconnectTimer != nil {
			gs.disconnectTimer.Stop()
		}
		vconn := vc
		gs.disconnectTimer = time.AfterFunc(15*time.Minute, func() {
			gs.intentionalLeave.Store(true)
			if err := vconn.Disconnect(); err != nil {
				log.Printf("idle disconnect: %v", err)
			}
		})
	}
}

// sendNowPlayingEmbed posts a "Now Playing" embed to the text channel.
// If the track has no metadata (direct URL, no search), only the URL is shown.
func sendNowPlayingEmbed(s *discordgo.Session, textChannelID string, t Track) {
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
	_, _ = s.ChannelMessageSendEmbed(textChannelID, &discordgo.MessageEmbed{
		Title:       "🎵 Now Playing",
		Description: description,
		URL:         t.URL,
	})
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
func streamAudio(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string) error {
	dlCmd := exec.Command(dl,
		"--no-playlist",
		"-f", "bestaudio",
		"-o", "-",
		youtubeURL,
	)

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
	ffCmd.Stderr = io.Discard

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
		return fmt.Errorf("yt-dlp start: %w", err)
	}
	if err := ffCmd.Start(); err != nil {
		_ = dlCmd.Process.Kill()
		return fmt.Errorf("ffmpeg start: %w", err)
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
		if _, err := readOggPage(ffStdout); err != nil {
			if gs.skipInterrupt.Load() {
				break
			}
			return fmt.Errorf("ogg header page %d: %w", skip, err)
		}
	}

	// Stream audio pages until EOF or skip signal.
	for {
		if gs.skipInterrupt.Load() {
			break
		}

		page, err := readOggPage(ffStdout)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // normal end of stream
			}
			if gs.skipInterrupt.Load() {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return fmt.Errorf("ogg demux: %w", err)
		}

		for _, pkt := range page.packets {
			if gs.skipInterrupt.Load() {
				break
			}
			if len(pkt) > 0 {
				vc.OpusSend <- pkt
			}
		}
	}

	_ = dlCmd.Wait()
	if err := ffCmd.Wait(); err != nil && !gs.skipInterrupt.Load() {
		return fmt.Errorf("ffmpeg exit: %w", err)
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
			gs.interruptPlayback()
			_ = respondEphemeral(s, i, "Skipped.")
		case "stop":
			gs.resetPlaybackState()
			s.RLock()
			vc, ok := s.VoiceConnections[i.GuildID]
			s.RUnlock()
			if ok && vc != nil {
				gs.intentionalLeave.Store(true)
				_ = vc.Disconnect()
			}
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
		}
	}
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