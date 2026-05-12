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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
	"layeh.com/gopus"
)

type configStruct struct {
	Token     string `json:"token"`
	BotPrefix string `json:"botPrefix"`
	APIKey    string `json:"APIKey"`
}

type Song struct {
	guildId    string
	channelID  string
	youtubeURL string
}

var (
	Token           string
	BotPrefix       string
	APIKey          string
	config          *configStruct
	BotID           string
	songQueue       []Song
	isPlaying       bool
	disconnectTimer *time.Timer
	queueMu         sync.Mutex
	// guild IDs where the next bot voice leave was initiated by us (skip clearing queue for that event)
	intentionalVoiceLeave sync.Map

	playbackMu    sync.Mutex
	activeDlCmd   *exec.Cmd
	activeFfCmd   *exec.Cmd
	skipInterrupt atomic.Bool

	errNotInVoice      = errors.New("not in a voice channel")
	errPlaylistMaxSize = errors.New("playlist max size reached")
)

const maxPlaylistTracks = 200

const discordChoiceNameMax = 100

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "play",
		Description: "Play from YouTube — type to search and pick a result, or paste a URL / playlist",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:         discordgo.ApplicationCommandOptionString,
				Name:         "query",
				Description:  "Search (live suggestions), or paste watch / playlist URL",
				Required:     true,
				Autocomplete: true,
			},
		},
	},
	{
		Name:        "skip",
		Description: "Skip the current track",
	},
	{
		Name:        "stop",
		Description: "Stop playback and clear the queue",
	},
	{
		Name:        "shuffle",
		Description: "Shuffle the queue",
	},
}

// yt-dlp output basename; Glob("audio.mp3*") also catches partials (e.g. .part, .ytdl).
const tempAudioBaseName = "audio.mp3"
const tempAudioPattern = tempAudioBaseName + "*"

func removeTempAudioFiles() {
	matches, err := filepath.Glob(tempAudioPattern)
	if err != nil {
		log.Printf("glob temp audio: %v", err)
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			log.Printf("remove temp audio %q: %v", m, err)
		}
	}
}

func killPlaybackProcesses() {
	playbackMu.Lock()
	defer playbackMu.Unlock()
	if activeDlCmd != nil && activeDlCmd.Process != nil {
		_ = activeDlCmd.Process.Kill()
	}
	if activeFfCmd != nil && activeFfCmd.Process != nil {
		_ = activeFfCmd.Process.Kill()
	}
	activeDlCmd = nil
	activeFfCmd = nil
}

func interruptPlayback() {
	skipInterrupt.Store(true)
	killPlaybackProcesses()
}

// resetPlaybackState clears the queue, idle timer, and temp downloads. Call when stopping playback.
func resetPlaybackState() {
	killPlaybackProcesses()
	skipInterrupt.Store(false)
	queueMu.Lock()
	songQueue = nil
	isPlaying = false
	if disconnectTimer != nil {
		disconnectTimer.Stop()
		disconnectTimer = nil
	}
	queueMu.Unlock()
	removeTempAudioFiles()
}

func main() {
	GetConfig()
	Start()

	<-make(chan struct{})
	return
}

func Start() {
	session, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Printf("Couldn't initialize bot: %v", err)
		return
	}

	user, err := session.User("@me")
	if err != nil {
		log.Printf("Error getting user: %v", err)
		return
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
		_, err := s.ApplicationCommandBulkOverwrite(appID, "", slashCommands)
		if err != nil {
			log.Printf("register slash commands: %v", err)
		} else {
			log.Printf("Slash commands registered (/play shows live YouTube suggestions).")
		}
	})

	err = session.Open()
	if err != nil {
		log.Printf("Error creating session: %v", err)
		return
	}

	log.Printf("Bot initialized successfuly. 👾")
}

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

	Token = strings.TrimSpace(fileCfg.Token)
	BotPrefix = strings.TrimSpace(fileCfg.BotPrefix)
	APIKey = strings.TrimSpace(fileCfg.APIKey)

	// Env overrides file (recommended on Fly.io, Railway, VPS, Render workers, etc.).
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

func markIntentionalVoiceLeave(guildID string) {
	if guildID == "" {
		return
	}
	intentionalVoiceLeave.Store(guildID, struct{}{})
}

func consumeIntentionalVoiceLeave(guildID string) bool {
	if guildID == "" {
		return false
	}
	_, ok := intentionalVoiceLeave.LoadAndDelete(guildID)
	return ok
}

func stopQueueOnExternalVoiceLeave(s *discordgo.Session, guildID string) {
	resetPlaybackState()

	s.RLock()
	vc, ok := s.VoiceConnections[guildID]
	s.RUnlock()
	if ok && vc != nil {
		markIntentionalVoiceLeave(guildID)
		_ = vc.Disconnect()
	}
}

func botVoiceStateHandler(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil {
		return
	}
	if vs.UserID != BotID || BotID == "" {
		return
	}
	if vs.ChannelID != "" {
		return
	}
	if consumeIntentionalVoiceLeave(vs.GuildID) {
		return
	}
	log.Printf("Bot left voice in guild %s (user disconnect or kick); clearing queue.", vs.GuildID)
	stopQueueOnExternalVoiceLeave(s, vs.GuildID)
}

func extractYouTubeVideoID(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil {
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
	}
	return ""
}

// runPlayFlow queues audio from a search string or URL. On success for a single video it returns ""
// (prefix flow sends no extra message). Playlist and error paths return text to show the user.
func runPlayFlow(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, searchQuery string) string {
	if strings.Contains(searchQuery, "list=") {
		_, _ = s.ChannelMessageSend(textChannelID, "Resolving playlist (first songs queue as soon as each page loads)…")
		n, err := fetchPlaylistEnqueue(ctx, s, guildID, textChannelID, voiceChannelID, searchQuery)
		if err != nil {
			if errors.Is(err, errPlaylistMaxSize) {
				return fmt.Sprintf("Queued %d tracks (server limit is %d per playlist).", n, maxPlaylistTracks)
			}
			if n > 0 {
				return fmt.Sprintf("Loaded %d tracks, then stopped: %v", n, err)
			}
			log.Printf("playlist: %v", err)
			return fmt.Sprintf("Could not load that playlist: %v", err)
		}
		if n == 0 {
			return "That playlist has no playable videos."
		}
		return fmt.Sprintf("Queued %d tracks.", n)
	}

	videoURL, err := fetchYoutubeUrl(ctx, searchQuery)
	if err != nil {
		log.Printf("search: %v", err)
		return fmt.Sprintf("Could not find a video: %v", err)
	}
	_ = playMusic(s, guildID, textChannelID, voiceChannelID, videoURL)
	return ""
}

func handlePlayCommand(s *discordgo.Session, m *discordgo.MessageCreate, voiceChID string) {
	i := strings.Index(m.Content, "&play")
	searchQuery := strings.TrimSpace(m.Content[i+len("&play"):])
	if searchQuery == "" {
		_, _ = s.ChannelMessageSend(m.ChannelID, "Usage: `&play` search words, a video URL, or a playlist URL (`list=…`).")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	if msg := runPlayFlow(ctx, s, m.GuildID, m.ChannelID, voiceChID, searchQuery); msg != "" {
		_, _ = s.ChannelMessageSend(m.ChannelID, msg)
	}
}

// Handlers
func messageHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	// &play commands
	if m.Author.ID == BotID {
		return
	}

	if norm := strings.TrimSpace(m.Content); norm == "&skip" || norm == "&next" {
		interruptPlayback()
		return
	}

	if strings.TrimSpace(m.Content) == "&stop" {
		resetPlaybackState()
		s.RLock()
		vc, ok := s.VoiceConnections[m.GuildID]
		s.RUnlock()
		if ok && vc != nil {
			markIntentionalVoiceLeave(m.GuildID)
			_ = vc.Disconnect()
		}
		_, _ = s.ChannelMessageSend(m.ChannelID, "Stopped playback, cleared the queue, and removed temp audio files.")
		return
	}

	if strings.Contains(m.Content, "&play") {
		voiceChID, err := voiceChannelForUser(s, m.GuildID, m.Author.ID)
		if err != nil {
			msg := "Join a voice channel first, then use `&play`."
			if !errors.Is(err, errNotInVoice) {
				msg = "Could not load this server yet. Try `&play` again in a second."
			}
			_, _ = s.ChannelMessageSend(m.ChannelID, msg)
			return
		}
		go handlePlayCommand(s, m, voiceChID)
		return
	}

	// &shuffle commands
	if m.Content == "&shuffle" {
		queueMu.Lock()
		n := len(songQueue)
		if n > 1 {
			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			r.Shuffle(n, func(i, j int) { songQueue[i], songQueue[j] = songQueue[j], songQueue[i] })
		}
		queueMu.Unlock()
		_, err := s.ChannelMessageSend(m.ChannelID, "Song queue has been shuffled.")
		if err != nil {
			return
		}
	}
}

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
			Flags:     discordgo.MessageFlagsEphemeral,
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
			choices = nil
		}
		if choices == nil {
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		}
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{
				Choices: choices,
			},
		}); err != nil {
			log.Printf("autocomplete respond: %v", err)
		}

	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		if i.GuildID == "" {
			_ = respondEphemeral(s, i, "Use this bot in a server text channel.")
			return
		}
		switch data.Name {
		case "play":
			handleSlashPlay(s, i)
		case "skip":
			interruptPlayback()
			_ = respondEphemeral(s, i, "Skip requested.")
		case "stop":
			resetPlaybackState()
			s.RLock()
			vc, ok := s.VoiceConnections[i.GuildID]
			s.RUnlock()
			if ok && vc != nil {
				markIntentionalVoiceLeave(i.GuildID)
				_ = vc.Disconnect()
			}
			_ = respondEphemeral(s, i, "Stopped playback and cleared the queue.")
		case "shuffle":
			queueMu.Lock()
			n := len(songQueue)
			if n > 1 {
				r := rand.New(rand.NewSource(time.Now().UnixNano()))
				r.Shuffle(n, func(i, j int) { songQueue[i], songQueue[j] = songQueue[j], songQueue[i] })
			}
			queueMu.Unlock()
			_ = respondEphemeral(s, i, "Queue shuffled.")
		default:
			return
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
		_ = respondEphemeral(s, i, "Enter a search, pick a suggestion, or paste a YouTube URL or playlist link.")
		return
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		log.Printf("slash defer: %v", err)
		return
	}

	ch := i.ChannelID
	guildID := i.GuildID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		msg := runPlayFlow(ctx, s, guildID, ch, voiceChID, query)
		if msg == "" {
			msg = "Queued — playback will start in your voice channel."
		}
		_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: &msg,
		})
		if err != nil {
			log.Printf("slash edit: %v", err)
			_, _ = s.ChannelMessageSend(ch, msg)
		}
	}()
}

// Audio utils.

func truncateChoiceLabel(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}

// youtubeAutocompleteChoices returns up to 25 slash-autocomplete choices (title · channel, value = watch URL).
func youtubeAutocompleteChoices(ctx context.Context, query string) ([]*discordgo.ApplicationCommandOptionChoice, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []*discordgo.ApplicationCommandOptionChoice{}, nil
	}

	service, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return nil, err
	}

	call := service.Search.List([]string{"id", "snippet"}).
		Q(query).
		Type("video").
		MaxResults(25).
		Context(ctx)
	response, err := call.Do()
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
		label = truncateChoiceLabel(label, discordChoiceNameMax)
		watch := "https://www.youtube.com/watch?v=" + item.Id.VideoId
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
			Name:  label,
			Value: watch,
		})
	}
	return out, nil
}

func fetchYoutubeUrl(ctx context.Context, searchQuery string) (string, error) {
	searchQuery = strings.TrimSpace(searchQuery)
	if searchQuery == "" {
		return "", errors.New("empty query")
	}
	if id := extractYouTubeVideoID(searchQuery); id != "" {
		return "https://www.youtube.com/watch?v=" + id, nil
	}

	service, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return "", fmt.Errorf("youtube client: %w", err)
	}

	call := service.Search.List([]string{"id", "snippet"}).Q(searchQuery).MaxResults(5).Context(ctx)
	response, err := call.Do()
	if err != nil {
		return "", fmt.Errorf("youtube search: %w", err)
	}
	if response == nil || len(response.Items) == 0 {
		return "", errors.New("no videos found")
	}

	firstItem := response.Items[0]
	if firstItem.Id == nil || firstItem.Id.Kind != "youtube#video" || firstItem.Id.VideoId == "" {
		return "", errors.New("no video result for that search")
	}

	return "https://www.youtube.com/watch?v=" + firstItem.Id.VideoId, nil
}

func fetchPlaylistEnqueue(ctx context.Context, s *discordgo.Session, guildID, textChannelID, voiceChannelID, playlistURL string) (int, error) {
	playlistURL = strings.TrimSpace(playlistURL)
	u, err := url.Parse(playlistURL)
	if err != nil {
		return 0, fmt.Errorf("invalid URL: %w", err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return 0, err
	}
	playlistID := strings.TrimSpace(q.Get("list"))
	if playlistID == "" {
		return 0, errors.New("no list= in link — open the playlist on YouTube and paste the full URL")
	}

	service, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		return 0, fmt.Errorf("youtube client: %w", err)
	}

	total := 0
	call := service.PlaylistItems.List([]string{"contentDetails"}).PlaylistId(playlistID).MaxResults(50).Context(ctx)
	err = call.Pages(ctx, func(page *youtube.PlaylistItemListResponse) error {
		for _, item := range page.Items {
			if item.ContentDetails == nil || item.ContentDetails.VideoId == "" {
				continue
			}
			if total >= maxPlaylistTracks {
				return errPlaylistMaxSize
			}
			videoURL := "https://www.youtube.com/watch?v=" + item.ContentDetails.VideoId
			_ = playMusic(s, guildID, textChannelID, voiceChannelID, videoURL)
			total++
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errPlaylistMaxSize) {
			return total, errPlaylistMaxSize
		}
		return total, err
	}
	return total, nil
}

func ytDownloaderPath() (string, error) {
	for _, name := range []string{"yt-dlp", "youtube-dl"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", errors.New("yt-dlp or youtube-dl not found in PATH (install yt-dlp: winget install yt-dlp.yt-dlp)")
}

func playMusic(s *discordgo.Session, guildID, textChannelID, voiceChannelID, youtubeURL string) error {
	queueMu.Lock()
	songQueue = append(songQueue, Song{guildId: guildID, channelID: voiceChannelID, youtubeURL: youtubeURL})
	shouldStart := !isPlaying
	if shouldStart {
		isPlaying = true
	}
	queueMu.Unlock()
	if shouldStart {
		go playNextSong(s, textChannelID)
	}
	return nil
}

func playNextSong(s *discordgo.Session, textChannelID string) {
	chID := textChannelID
	audioFile := tempAudioBaseName

	for {
		queueMu.Lock()
		if len(songQueue) == 0 {
			isPlaying = false
			if disconnectTimer != nil {
				disconnectTimer.Stop()
				disconnectTimer = nil
			}
			queueMu.Unlock()
			removeTempAudioFiles()
			return
		}
		song := songQueue[0]
		songQueue = songQueue[1:]
		queueMu.Unlock()

		skipInterrupt.Store(false)

		vc, err := s.ChannelVoiceJoin(song.guildId, song.channelID, false, true)
		if err != nil {
			log.Printf("voice join: %v", err)
			queueMu.Lock()
			songQueue = append([]Song{song}, songQueue...)
			isPlaying = false
			queueMu.Unlock()
			removeTempAudioFiles()
			_, _ = s.ChannelMessageSend(chID, fmt.Sprintf("Could not join voice: %v", err))
			return
		}

		removeTempAudioFiles()
		dl, err := ytDownloaderPath()
		if err != nil {
			log.Printf("downloader: %v", err)
			_, _ = s.ChannelMessageSend(chID, "yt-dlp and ffmpeg must be installed and on PATH. Fix that, then try `&play` again.")
			markIntentionalVoiceLeave(song.guildId)
			_ = vc.Disconnect()
			resetPlaybackState()
			return
		}

		cmd := exec.Command(dl, "-x", "--audio-format", "mp3", "-o", audioFile, song.youtubeURL)
		playbackMu.Lock()
		activeDlCmd = cmd
		playbackMu.Unlock()
		err = cmd.Run()
		playbackMu.Lock()
		activeDlCmd = nil
		playbackMu.Unlock()
		if err != nil {
			removeTempAudioFiles()
			if skipInterrupt.Load() {
				skipInterrupt.Store(false)
				_, _ = s.ChannelMessageSend(chID, "Skipped.")
				markIntentionalVoiceLeave(song.guildId)
				_ = vc.Disconnect()
				continue
			}
			log.Printf("yt-dlp: %v", err)
			_, _ = s.ChannelMessageSend(chID, "Skipping a track (download or extract failed).")
			markIntentionalVoiceLeave(song.guildId)
			_ = vc.Disconnect()
			removeTempAudioFiles()
			continue
		}

		if _, err = s.ChannelMessageSend(chID, fmt.Sprintf("Now playing: %v", song.youtubeURL)); err != nil {
			log.Printf("notify: %v", err)
		}

		err = playAudioFile(vc, audioFile)
		if skipInterrupt.Load() {
			skipInterrupt.Store(false)
			_, _ = s.ChannelMessageSend(chID, "Skipped.")
		} else if err != nil {
			log.Printf("playback: %v", err)
			_, _ = s.ChannelMessageSend(chID, "Playback error on a track; continuing if there are more.")
		}

		removeTempAudioFiles()

		if disconnectTimer != nil {
			disconnectTimer.Stop()
		}
		gid := song.guildId
		vconn := vc
		disconnectTimer = time.AfterFunc(15*time.Minute, func() {
			markIntentionalVoiceLeave(gid)
			removeTempAudioFiles()
			if err := vconn.Disconnect(); err != nil {
				log.Printf("disconnect: %v", err)
			}
		})
	}
}

func playAudioFile(vc *discordgo.VoiceConnection, filename string) error {
	cmd := exec.Command("ffmpeg", "-i", filename, "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
	stdout, err := cmd.StdoutPipe()

	if err != nil {
		log.Printf("Error creating stdout pipe: %v", err)
		return err
	}

	if err = cmd.Start(); err != nil {
		log.Printf("Error starting command: %v", err)
		return err
	}

	playbackMu.Lock()
	activeFfCmd = cmd
	playbackMu.Unlock()
	defer func() {
		playbackMu.Lock()
		if activeFfCmd == cmd {
			activeFfCmd = nil
		}
		playbackMu.Unlock()
	}()

	opusEncoder, _ := gopus.NewEncoder(48000, 2, gopus.Audio)

	for {
		data := make([]byte, 960*2*2)
		n, err := stdout.Read(data)
		if err != nil {
			if err != io.EOF {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
				if skipInterrupt.Load() {
					return nil
				}
				log.Printf("Error reading from stdout: %v", err)
				return err
			}
			break
		}

		data = data[:n]
		pcm := make([]int16, len(data)/2)
		for i := 0; i < len(data); i += 2 {
			pcm[i/2] = int16(binary.LittleEndian.Uint16(data[i : i+2]))
		}

		opusData, err := opusEncoder.Encode(pcm, 960, 5760)
		if err != nil {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			if skipInterrupt.Load() {
				return nil
			}
			log.Printf("Error encoding PCM data: %v", err)
			return err
		}

		vc.OpusSend <- opusData
	}

	if err := cmd.Wait(); err != nil {
		if skipInterrupt.Load() {
			return nil
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}
