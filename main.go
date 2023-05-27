package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
	"io"
	"layeh.com/gopus"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	err := GetConfig()
	if err != nil {
		log.Printf("Error getting config: %v", err)
		return
	}

	Start()

	<-make(chan struct{})
	return
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

func GetConfig() error {
	log.Printf("Received request to get configuration...")

	file, err := os.ReadFile("./config.json")
	if err != nil {
		log.Printf("Couldn't get configuration: %v", err)
		return err
	}

	err = json.Unmarshal(file, &config)

	if err != nil {
		log.Printf("Couldn't get configuration: %v", err)
		return err
	}

	Token = config.Token
	BotPrefix = config.BotPrefix
	APIKey = config.APIKey

	log.Printf("Configuration loaded succesfuly!")

	return nil
}

func Start() {
	session, err := discordgo.New("Bot " + config.Token)
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

	session.AddHandler(messageHandler)

	err = session.Open()
	if err != nil {
		log.Printf("Error creating session: %v", err)
		return
	}

	log.Printf("Bot initialized successfuly!")
}

func messageHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == BotID {
		return
	}

	guild, _ := s.State.Guild(m.GuildID)
	channelID := m.ChannelID

	if m.Content == "sus" {
		_, _ = s.ChannelMessageSend(m.ChannelID, "muy sus")
	}

	if strings.Contains(m.Content, "&play") {
		searchQuery := strings.TrimPrefix(m.Content, "&play")

		youtubeURL, err := fetchYoutubeUrl(searchQuery)
		err = playMusic(s, guild.ID, channelID, youtubeURL)
		if err != nil {
			log.Printf("Error playing sound: %v", err)
		}
	}
}

func fetchYoutubeUrl(searchQuery string) (string, error) {
	ctx := context.Background()

	service, err := youtube.NewService(ctx, option.WithAPIKey(APIKey))
	if err != nil {
		log.Printf("Error creating Youtube client: %v", err)
	}

	call := service.Search.List([]string{"id", "snippet"}).Q(searchQuery).MaxResults(5)

	response, err := call.Do()
	if err != nil {
		log.Printf("Error making search API call: %v", err)
	}

	if len(response.Items) == 0 {
		return "", errors.New("no videos found")
	}

	firstItem := response.Items[0]
	if firstItem.Id.Kind != "youtube#video" {
		return "", errors.New("first item is not a video")
	}

	videoId := firstItem.Id.VideoId
	videoURL := "https://www.youtube.com/watch?v=" + videoId

	return videoURL, nil
}

func playMusic(s *discordgo.Session, guildId, channelID, youtubeURL string) error {
	songQueue = append(songQueue, Song{guildId: guildId, channelID: channelID, youtubeURL: youtubeURL})
	log.Printf("Added song to list!")
	if !isPlaying {
		go playNextSong(s)
	}
	return nil
}

func playNextSong(s *discordgo.Session) {
	isPlaying = true
	song := songQueue[0]
	songQueue = songQueue[1:]

	audioFile := "audio.mp3"
	cmd := exec.Command("youtube-dl", "-x", "--audio-format", "mp3", "-o", audioFile, song.youtubeURL)
	err := cmd.Run()
	if err != nil {
		log.Printf("Failed to download audio: %v", err)
		return
	}

	vc, err := s.ChannelVoiceJoin(song.guildId, song.channelID, false, true)
	if err != nil {
		log.Printf("Failed to join voice channel: %v", err)
		return
	}

	err = playAudioFile(vc, audioFile)
	if err != nil {
		log.Printf("Failed to play audio file: %v", err)
		return
	}

	removeAudioFile()

	if disconnectTimer != nil {
		disconnectTimer.Stop()
	}

	disconnectTimer = time.AfterFunc(15*time.Minute, func() {
		err = vc.Disconnect()
		if err != nil {
			log.Printf("Failed to disconnect from channel: %v", err)
			return
		}
	})

	if len(songQueue) > 0 {
		playNextSong(s)
	} else {
		isPlaying = false
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

	opusEncoder, _ := gopus.NewEncoder(48000, 2, gopus.Audio)

	for {
		data := make([]byte, 960*2*2)
		n, err := stdout.Read(data)
		if err != nil {
			if err != io.EOF {
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
			log.Printf("Error encoding PCM data: %v", err)
			return err
		}

		vc.OpusSend <- opusData
	}

	return nil
}

func removeAudioFile() {
	err := os.Remove("audio.mp3")
	if err != nil {
		log.Printf("Failed to delete audio file: %v", err)
	}
}
