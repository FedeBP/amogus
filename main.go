package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
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
	"layeh.com/gopus"
)

// ---------------------------------------------------------------------------
// Config
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

func GetConfig() {
	log.Printf("Reading configuration...")
	var fileCfg configStruct
	if data, err := os.ReadFile("./config.json"); err == nil {
		if err := json.Unmarshal(data, &fileCfg); err != nil {
			log.Fatalf("config.json: %v", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("config.json: %v", err)
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
		log.Fatal("Missing Discord token: set DISCORD_BOT_TOKEN or token in config.json")
	}
	if APIKey == "" {
		log.Fatal("Missing YouTube API key: set YOUTUBE_API_KEY or APIKey in config.json")
	}
	config = &configStruct{Token: Token, BotPrefix: BotPrefix, APIKey: APIKey}
	log.Printf("Configuration loaded.")
}

// ---------------------------------------------------------------------------
// Per-guild state
// ---------------------------------------------------------------------------

type Song struct {
	guildID    string
	channelID  string
	youtubeURL string
}

// guildState holds all mutable playback state for one Discord server.
// Keeping this per-guild means multiple servers never interfere with each other.
type guildState struct {
	mu               sync.Mutex
	songQueue        []Song
	isPlaying        bool
	disconnectTimer  *time.Timer
	activeDlCmd      *exec.Cmd
	activeFfCmd      *exec.Cmd
	skipInterrupt    atomic.Bool
	intentionalLeave atomic.Bool
}

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

func (gs *guildState) interruptPlayback() {
	gs.skipInterrupt.Store(true)
	gs.killPlaybackProcesses()
}

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

var (
	guildsMu sync.Mutex
	guilds   = make(map[string]*guildState)
)

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
// Globals
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
	// Discord requires 48 kHz stereo Opus; 960 samples = 20 ms per frame.
	opusFrameSize    = 960
	opusChannels     = 2
	opusMaxFrameSize = 5760
)

// ---------------------------------------------------------------------------
// Bot setup
// ---------------------------------------------------------------------------

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "play",
		Description: "Play from YouTube Music — search or paste a URL / playlist",
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
}

func main() {
	GetConfig()
	Start()
	<-make(chan struct{})
}

func Start() {
	session, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Couldn't initialize bot: %v", err)
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

	session.AddHandler(messageHandler)
	session.AddHandler(botVoiceStateHandler)
	session.AddHandler(interactionHandler)
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
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
	log.Printf("Bot initialized successfully. 👾")
}

// ---------------------------------------------------------------------------
// Voice helpers
// ---------------------------------------------------------------------------

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
// by someone else and clears the queue for that guild.
func botVoiceStateHandler(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil || vs.UserID != BotID || BotID == "" {
		return
	}
	if vs.ChannelID != "" {
		return // bot joined or moved, not a leave
	}
	gs := getGuildState(vs.GuildID)
	if gs.intentionalLeave.CompareAndSwap(true, false) {
		return // we triggered this disconnect ourselves
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
// URL / search helpers
// ---------------------------------------------------------------------------

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

func ytDownloaderPath() (string, error) {
	for _, name := range []string{"yt-dlp", "youtube-dl"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("yt-dlp not found in PATH")
}

// fetchYouTubeURL resolves a search query or direct URL to a playable watch URL.
//
// For text searches it uses yt-dlp's ytmsearch: extractor, which queries
// YouTube Music directly. This returns audio tracks instead of music videos,
// skipping intros, outros, and unrelated content, and eliminates the YouTube
// Data API quota cost for every play command.
//
// Direct URLs (any recognised YouTube host) bypass search entirely.
func fetchYouTubeURL(ctx context.Context, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("empty query")
	}
	if id := extractYouTubeVideoID(query); id != "" {
		return "https://www.youtube.com/watch?v=" + id, nil
	}

	dl, err := ytDownloaderPath()
	if err != nil {
		return "", err
	}

	// ytmsearch1: returns the top YouTube Music result.
	// --get-id prints just the video ID without downloading anything (~0.5-1 s).
	cmd := exec.CommandContext(ctx, dl,
		"--get-id",
		"--no-playlist",
		"ytmsearch1:"+query,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ytmsearch: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("no results from YouTube Music search")
	}
	return "https://www.youtube.com/watch?v=" + id, nil
}

// youtubeAutocompleteChoices returns up to 25 slash autocomplete options using
// the YouTube Data API (kept only here for its rich title+channel metadata).
func youtubeAutocompleteChoices(ctx context.Context, query string) ([]*discordgo.ApplicationCommandOptionChoice, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []*discordgo.ApplicationCommandOptionChoice{}, nil
	}
	service, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return nil, err
	}
	response, err := service.Search.List([]string{"id", "snippet"}).
		Q(query).Type("video").MaxResults(25).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if response == nil || len(response.Items) == 0 {
		return []*discordgo.ApplicationCommandOptionChoice{}, nil
	}
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(response.Items))
	for _, item := range response.Items {
		if item.Id == nil || item.Id.Kind != "youtube#video" || item.Id.VideoId == "" || item.Snippet == nil {
			continue
		}
		title := strings.TrimSpace(item.Snippet.Title)
		ch := strings.TrimSpace(item.Snippet.ChannelTitle)
		label := title
		if ch != "" {
			label = title + " · " + ch
		}
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
			Name:  truncateChoiceLabel(label, discordChoiceNameMax),
			Value: "https://www.youtube.com/watch?v=" + item.Id.VideoId,
		})
	}
	return out, nil
}

func truncateChoiceLabel(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}

// fetchPlaylistEnqueue resolves a YouTube playlist and enqueues each video.
// It pages through results so playback can start before the full list is fetched.
func fetchPlaylistEnqueue(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, playlistURL string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(playlistURL))
	if err != nil {
		return 0, fmt.Errorf("invalid URL: %w", err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return 0, err
	}
	playlistID := strings.TrimSpace(q.Get("list"))
	if playlistID == "" {
		return 0, errors.New("no list= in URL — paste the full playlist URL from YouTube")
	}
	service, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return 0, fmt.Errorf("youtube client: %w", err)
	}
	total := 0
	call := service.PlaylistItems.List([]string{"contentDetails"}).
		PlaylistId(playlistID).MaxResults(50).Context(ctx)
	err = call.Pages(ctx, func(page *youtube.PlaylistItemListResponse) error {
		for _, item := range page.Items {
			if item.ContentDetails == nil || item.ContentDetails.VideoId == "" {
				continue
			}
			if total >= maxPlaylistTracks {
				return errPlaylistMaxSize
			}
			_ = enqueueURL(s, guildID, textChannelID, voiceChannelID,
				"https://www.youtube.com/watch?v="+item.ContentDetails.VideoId)
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

func runPlayFlow(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, query string) string {
	if strings.Contains(query, "list=") {
		_, _ = s.ChannelMessageSend(textChannelID, "Resolving playlist…")
		n, err := fetchPlaylistEnqueue(ctx, s, guildID, textChannelID, voiceChannelID, query)
		if err != nil {
			if errors.Is(err, errPlaylistMaxSize) {
				return fmt.Sprintf("Queued %d tracks (limit is %d per playlist).", n, maxPlaylistTracks)
			}
			if n > 0 {
				return fmt.Sprintf("Loaded %d tracks, then hit an error: %v", n, err)
			}
			log.Printf("playlist: %v", err)
			return fmt.Sprintf("Could not load playlist: %v", err)
		}
		if n == 0 {
			return "That playlist has no playable videos."
		}
		return fmt.Sprintf("Queued %d tracks.", n)
	}

	videoURL, err := fetchYouTubeURL(ctx, query)
	if err != nil {
		log.Printf("search: %v", err)
		return fmt.Sprintf("Could not find a video: %v", err)
	}
	_ = enqueueURL(s, guildID, textChannelID, voiceChannelID, videoURL)
	return ""
}

// enqueueURL adds a song to the guild queue and starts the playback goroutine
// if nothing is currently playing.
func enqueueURL(s *discordgo.Session, guildID, textChannelID, voiceChannelID, youtubeURL string) error {
	gs := getGuildState(guildID)
	gs.mu.Lock()
	gs.songQueue = append(gs.songQueue, Song{guildID: guildID, channelID: voiceChannelID, youtubeURL: youtubeURL})
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
// Playback
// ---------------------------------------------------------------------------

// playNextSong is the main playback loop for a guild. It runs in its own
// goroutine and processes the queue until empty.
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

	// Create the Opus encoder once for this playback session and reuse it
	// across all songs — avoids repeated allocation and encoder state setup.
	opusEncoder, err := gopus.NewEncoder(48000, opusChannels, gopus.Audio)
	if err != nil {
		log.Printf("create opus encoder: %v", err)
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
			gs.mu.Lock()
			gs.songQueue = append([]Song{song}, gs.songQueue...)
			gs.isPlaying = false
			gs.mu.Unlock()
			_, _ = s.ChannelMessageSend(textChannelID, fmt.Sprintf("Could not join voice: %v", err))
			return
		}

		_, _ = s.ChannelMessageSend(textChannelID, fmt.Sprintf("Now playing: %v", song.youtubeURL))

		if err := streamAudio(gs, vc, dl, song.youtubeURL, opusEncoder); err != nil {
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

		// Reset the idle-disconnect timer after each track.
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

// streamAudio pipes yt-dlp directly into ffmpeg and sends the resulting Opus
// frames to Discord. No temp file is written — audio starts playing as soon
// as the first frames arrive, cutting the old 10-15 s wait to ~1-2 s.
//
//	yt-dlp stdout ──pipe──▶ ffmpeg stdin → ffmpeg stdout ──▶ opusEncoder ──▶ Discord
//
// The Opus encoder is passed in and reused across songs.
// PCM and byte buffers are pre-allocated once and reused every frame,
// eliminating the two make() calls per frame that existed in the original code.
func streamAudio(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string, opusEncoder *gopus.Encoder) error {
	// yt-dlp: select best audio format, write raw stream to stdout.
	dlCmd := exec.Command(dl,
		"--no-playlist",
		"-f", "bestaudio",
		"-o", "-",
		youtubeURL,
	)

	// ffmpeg: read from stdin, output raw signed 16-bit little-endian PCM.
	ffCmd := exec.Command("ffmpeg",
		"-i", "pipe:0",
		"-f", "s16le",
		"-ar", "48000",
		"-ac", "2",
		"pipe:1",
	)
	ffCmd.Stderr = io.Discard // suppress ffmpeg's verbose stderr

	// Wire yt-dlp stdout → ffmpeg stdin.
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

	// Register processes so skip/stop can kill them immediately.
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

	// Pre-allocate buffers once; reuse every frame.
	rawBuf := make([]byte, opusFrameSize*opusChannels*2) // 16-bit = 2 bytes/sample
	pcmBuf := make([]int16, opusFrameSize*opusChannels)

	for {
		if gs.skipInterrupt.Load() {
			break
		}

		_, err := io.ReadFull(ffStdout, rawBuf)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // end of stream — normal exit
			}
			if gs.skipInterrupt.Load() {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return fmt.Errorf("read pcm: %w", err)
		}

		// Decode little-endian bytes → int16 PCM in-place.
		for i := range pcmBuf {
			pcmBuf[i] = int16(binary.LittleEndian.Uint16(rawBuf[i*2 : i*2+2]))
		}

		opusData, err := opusEncoder.Encode(pcmBuf, opusFrameSize, opusMaxFrameSize)
		if err != nil {
			if gs.skipInterrupt.Load() {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return fmt.Errorf("opus encode: %w", err)
		}

		vc.OpusSend <- opusData
	}

	// Wait for both processes to exit (or after kill on skip/stop).
	_ = dlCmd.Wait()
	if err := ffCmd.Wait(); err != nil && !gs.skipInterrupt.Load() {
		return fmt.Errorf("ffmpeg exit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Message handler
// ---------------------------------------------------------------------------

func messageHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == BotID {
		return
	}
	norm := strings.TrimSpace(m.Content)

	switch {
	case norm == "&skip" || norm == "&next":
		getGuildState(m.GuildID).interruptPlayback()

	case norm == "&stop":
		gs := getGuildState(m.GuildID)
		gs.resetPlaybackState()
		s.RLock()
		vc, ok := s.VoiceConnections[m.GuildID]
		s.RUnlock()
		if ok && vc != nil {
			gs.intentionalLeave.Store(true)
			_ = vc.Disconnect()
		}
		_, _ = s.ChannelMessageSend(m.ChannelID, "Stopped playback and cleared the queue.")

	case norm == "&shuffle":
		gs := getGuildState(m.GuildID)
		gs.mu.Lock()
		n := len(gs.songQueue)
		if n > 1 {
			rngMu.Lock()
			rng.Shuffle(n, func(i, j int) { gs.songQueue[i], gs.songQueue[j] = gs.songQueue[j], gs.songQueue[i] })
			rngMu.Unlock()
		}
		gs.mu.Unlock()
		_, _ = s.ChannelMessageSend(m.ChannelID, "Queue shuffled.")

	case strings.Contains(norm, "&play"):
		voiceChID, err := voiceChannelForUser(s, m.GuildID, m.Author.ID)
		if err != nil {
			msg := "Join a voice channel first, then use `&play`."
			if !errors.Is(err, errNotInVoice) {
				msg = "Could not read server state. Try again in a moment."
			}
			_, _ = s.ChannelMessageSend(m.ChannelID, msg)
			return
		}
		go func() {
			query := strings.TrimSpace(norm[strings.Index(norm, "&play")+len("&play"):])
			if query == "" {
				_, _ = s.ChannelMessageSend(m.ChannelID, "Usage: `&play <search or URL>`")
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			if msg := runPlayFlow(ctx, s, m.GuildID, m.ChannelID, voiceChID, query); msg != "" {
				_, _ = s.ChannelMessageSend(m.ChannelID, msg)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// Interaction (slash command) handler
// ---------------------------------------------------------------------------

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {

	case discordgo.InteractionApplicationCommandAutocomplete:
		data := i.ApplicationCommandData()
		if data.Name != "play" || APIKey == "" {
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
				rng.Shuffle(n, func(i, j int) { gs.songQueue[i], gs.songQueue[j] = gs.songQueue[j], gs.songQueue[i] })
				rngMu.Unlock()
			}
			gs.mu.Unlock()
			_ = respondEphemeral(s, i, "Queue shuffled.")
		}
	}
}

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

	// Defer immediately so Discord doesn't time out while we search/resolve.
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