package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
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
}
