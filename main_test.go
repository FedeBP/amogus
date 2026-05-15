package main

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestResetPlaybackStateClearsAutoplayAndSessionState(t *testing.T) {
	gs := &guildState{
		songQueue: []Song{
			{guildID: "guild", channelID: "voice", track: Track{URL: "https://www.youtube.com/watch?v=abc123"}},
		},
		isPlaying:          true,
		autoplayEnabled:    true,
		lastPlayed:         Track{URL: "https://www.youtube.com/watch?v=def456"},
		lastVoiceChannelID: "voice",
		recentVideoIDs:     []string{"abc123", "def456"},
		recentTracks: []Track{
			{URL: "https://www.youtube.com/watch?v=abc123", Title: "Victory Lap Five", Artist: "Fred again.."},
		},
		disconnectTimer: time.AfterFunc(time.Hour, func() {}),
	}

	gs.resetPlaybackState()

	if len(gs.songQueue) != 0 {
		t.Fatalf("songQueue length = %d, want 0", len(gs.songQueue))
	}
	if gs.isPlaying {
		t.Fatal("isPlaying = true, want false")
	}
	if gs.autoplayEnabled {
		t.Fatal("autoplayEnabled = true, want false")
	}
	if gs.lastPlayed != (Track{}) {
		t.Fatalf("lastPlayed = %#v, want zero value", gs.lastPlayed)
	}
	if gs.lastVoiceChannelID != "" {
		t.Fatalf("lastVoiceChannelID = %q, want empty", gs.lastVoiceChannelID)
	}
	if len(gs.recentVideoIDs) != 0 {
		t.Fatalf("recentVideoIDs length = %d, want 0", len(gs.recentVideoIDs))
	}
	if len(gs.recentTracks) != 0 {
		t.Fatalf("recentTracks length = %d, want 0", len(gs.recentTracks))
	}
	if gs.disconnectTimer != nil {
		t.Fatal("disconnectTimer was not cleared")
	}
}

func TestStopAfterAutoplayErrorKeepsLoopAliveForQueuedTrack(t *testing.T) {
	gs := &guildState{
		songQueue: []Song{
			{guildID: "guild", channelID: "voice", track: Track{URL: "https://www.youtube.com/watch?v=abc123"}},
		},
		isPlaying: true,
	}
	generation := gs.playbackGeneration.Load()

	if gs.stopAfterAutoplayError(generation) {
		t.Fatal("stopAfterAutoplayError() = true, want false with queued track")
	}
	if !gs.isPlaying {
		t.Fatal("isPlaying = false, want true")
	}
}

func TestAutoplayNearDuplicateCatchesTitleVariants(t *testing.T) {
	history := []Track{
		{URL: "https://www.youtube.com/watch?v=abc123", Title: "Victory Lap Five", Artist: "Fred again.."},
	}
	candidate := Track{
		URL:    "https://www.youtube.com/watch?v=def456",
		Title:  "Victory Lap",
		Artist: "Fred again..",
	}

	if !autoplayNearDuplicate(candidate, history) {
		t.Fatal("autoplayNearDuplicate() = false, want true for same-artist title variant")
	}
}

func TestAutoplayNearDuplicateIgnoresCommonTitleFromDifferentArtist(t *testing.T) {
	history := []Track{
		{URL: "https://www.youtube.com/watch?v=abc123", Title: "Home", Artist: "Artist A"},
	}
	candidate := Track{
		URL:    "https://www.youtube.com/watch?v=def456",
		Title:  "Home",
		Artist: "Artist B",
	}

	if autoplayNearDuplicate(candidate, history) {
		t.Fatal("autoplayNearDuplicate() = true, want false for different-artist common title")
	}
}

func TestRememberTrackLockedKeepsRecentTrackMetadata(t *testing.T) {
	gs := &guildState{}

	gs.rememberTrackLocked(Track{
		URL:    "https://www.youtube.com/watch?v=abc123",
		Title:  "Victory Lap Five",
		Artist: "Fred again..",
	})

	if len(gs.recentVideoIDs) != 1 {
		t.Fatalf("recentVideoIDs length = %d, want 1", len(gs.recentVideoIDs))
	}
	if len(gs.recentTracks) != 1 {
		t.Fatalf("recentTracks length = %d, want 1", len(gs.recentTracks))
	}
	if got := normalizedAutoplayTitle(gs.recentTracks[0].Title); got != "victory lap five" {
		t.Fatalf("normalized recent title = %q, want %q", got, "victory lap five")
	}
}

func TestStopAfterAutoplayErrorStopsEmptyLoop(t *testing.T) {
	gs := &guildState{isPlaying: true}
	generation := gs.playbackGeneration.Load()

	if !gs.stopAfterAutoplayError(generation) {
		t.Fatal("stopAfterAutoplayError() = false, want true with empty queue")
	}
	if gs.isPlaying {
		t.Fatal("isPlaying = true, want false")
	}
}

func TestSendOpusPacketReturnsWhenSkipIsRequested(t *testing.T) {
	gs := &guildState{}
	gs.skipInterrupt.Store(true)
	vc := &discordgo.VoiceConnection{OpusSend: make(chan []byte)}

	done := make(chan error, 1)
	go func() {
		done <- sendOpusPacket(gs, vc, []byte{1, 2, 3})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sendOpusPacket() error = %v, want nil", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("sendOpusPacket blocked after skip was requested")
	}
}
