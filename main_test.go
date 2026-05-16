package main

import (
	"fmt"
	"strings"
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

func TestIdleDisconnectTimeoutIsTenMinutes(t *testing.T) {
	if idleDisconnectTimeout != 10*time.Minute {
		t.Fatalf("idleDisconnectTimeout = %s, want 10m0s", idleDisconnectTimeout)
	}
}

func TestBuildNowPlayingComponentsAddsControlButtons(t *testing.T) {
	components := buildNowPlayingComponents()
	if len(components) != 1 {
		t.Fatalf("components length = %d, want 1", len(components))
	}
	row, ok := components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("component type = %T, want discordgo.ActionsRow", components[0])
	}

	expected := []struct {
		label    string
		customID string
		style    discordgo.ButtonStyle
	}{
		{label: "Skip", customID: nowPlayingButtonSkip, style: discordgo.PrimaryButton},
		{label: "Stop", customID: nowPlayingButtonStop, style: discordgo.DangerButton},
		{label: "Queue", customID: nowPlayingButtonQueue, style: discordgo.SecondaryButton},
		{label: "Autoplay", customID: nowPlayingButtonAutoplay, style: discordgo.SecondaryButton},
	}

	if len(row.Components) != len(expected) {
		t.Fatalf("row components length = %d, want %d", len(row.Components), len(expected))
	}
	for i, want := range expected {
		button, ok := row.Components[i].(discordgo.Button)
		if !ok {
			t.Fatalf("button %d type = %T, want discordgo.Button", i, row.Components[i])
		}
		if button.Label != want.label || button.CustomID != want.customID || button.Style != want.style {
			t.Fatalf("button %d = %#v, want label=%q customID=%q style=%d", i, button, want.label, want.customID, want.style)
		}
	}
}

func TestSetAutoplayTogglesAndAppliesExplicitState(t *testing.T) {
	gs := &guildState{}

	if enabled := setAutoplay(gs, nil); !enabled {
		t.Fatal("setAutoplay toggle = false, want true")
	}
	if !gs.autoplayEnabled {
		t.Fatal("autoplayEnabled = false, want true")
	}

	requested := false
	if enabled := setAutoplay(gs, &requested); enabled {
		t.Fatal("setAutoplay explicit false = true, want false")
	}
	if gs.autoplayEnabled {
		t.Fatal("autoplayEnabled = true, want false")
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

func TestChooseAutoplayTrackPrefersFreshCandidate(t *testing.T) {
	history := []Track{
		{URL: "https://www.youtube.com/watch?v=abc123", Title: "Victory Lap Five", Artist: "Fred again.."},
	}
	results := []SearchResult{
		{VideoID: "duplicate", Title: "Victory Lap", Artist: "Fred again..", Duration: "2:46"},
		{VideoID: "fresh", Title: "Different Song", Artist: "Another Artist", Duration: "3:10"},
	}

	track, err := chooseAutoplayTrack(results, nil, history)
	if err != nil {
		t.Fatalf("chooseAutoplayTrack() error = %v, want nil", err)
	}
	if got := trackVideoID(track); got != "fresh" {
		t.Fatalf("chosen video ID = %q, want fresh", got)
	}
}

func TestChooseAutoplayTrackKeepsExcludedVideosBlocked(t *testing.T) {
	results := []SearchResult{
		{VideoID: "blocked", Title: "Blocked Song", Artist: "Artist", Duration: "2:46"},
	}

	_, err := chooseAutoplayTrack(results, map[string]struct{}{"blocked": {}}, nil)
	if err == nil {
		t.Fatal("chooseAutoplayTrack() error = nil, want error when all results are excluded")
	}
}

func TestChooseAutoplayTrackRejectsOnlyNearDuplicates(t *testing.T) {
	history := []Track{
		{URL: "https://www.youtube.com/watch?v=abc123", Title: "Victory Lap Five", Artist: "Fred again.."},
	}
	results := []SearchResult{
		{VideoID: "duplicate", Title: "Victory Lap", Artist: "Fred again..", Duration: "2:46"},
	}

	_, err := chooseAutoplayTrack(results, nil, history)
	if err == nil {
		t.Fatal("chooseAutoplayTrack() error = nil, want error when only near-duplicates are available")
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
	generation := gs.playbackGeneration.Load()

	done := make(chan error, 1)
	go func() {
		done <- sendOpusPacket(gs, vc, []byte{1, 2, 3}, generation)
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

func TestSendOpusPacketReturnsWhenGenerationChanges(t *testing.T) {
	gs := &guildState{}
	vc := &discordgo.VoiceConnection{OpusSend: make(chan []byte)}
	generation := gs.playbackGeneration.Load()
	gs.playbackGeneration.Add(1)

	done := make(chan error, 1)
	go func() {
		done <- sendOpusPacket(gs, vc, []byte{1, 2, 3}, generation)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sendOpusPacket() error = %v, want nil", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("sendOpusPacket blocked after generation changed")
	}
}

func TestPrepareSkipWithAutoplayResetsPlayingState(t *testing.T) {
	gs := &guildState{
		isPlaying:          true,
		autoplayEnabled:    true,
		lastPlayed:         Track{URL: "https://www.youtube.com/watch?v=abc123", Title: "Song 1", Artist: "Artist"},
		lastVoiceChannelID: "voice",
	}
	oldGeneration := gs.playbackGeneration.Load()

	req, shouldAutoplay := gs.prepareSkip()

	if !shouldAutoplay {
		t.Fatal("prepareSkip() shouldAutoplay = false, want true")
	}
	if req.voiceChannelID != "voice" {
		t.Fatalf("voiceChannelID = %q, want voice", req.voiceChannelID)
	}
	if trackVideoID(req.seed) != "abc123" {
		t.Fatalf("seed video ID = %q, want abc123", trackVideoID(req.seed))
	}
	if gs.playbackGeneration.Load() == oldGeneration {
		t.Fatal("playbackGeneration did not change")
	}
	if gs.isPlaying {
		t.Fatal("isPlaying = true, want false")
	}
	if !gs.skipInterrupt.Load() {
		t.Fatal("skipInterrupt = false, want true")
	}
}

func TestPrepareSkipWithQueuedTrackUsesNormalSkip(t *testing.T) {
	gs := &guildState{
		isPlaying:          true,
		autoplayEnabled:    true,
		lastPlayed:         Track{URL: "https://www.youtube.com/watch?v=abc123", Title: "Song 1"},
		lastVoiceChannelID: "voice",
		songQueue: []Song{
			{track: Track{Title: "Queued Song"}},
		},
	}
	oldGeneration := gs.playbackGeneration.Load()

	_, shouldAutoplay := gs.prepareSkip()

	if shouldAutoplay {
		t.Fatal("prepareSkip() shouldAutoplay = true, want false with queued track")
	}
	if gs.playbackGeneration.Load() != oldGeneration {
		t.Fatal("playbackGeneration changed for normal skip")
	}
	if !gs.skipInterrupt.Load() {
		t.Fatal("skipInterrupt = false, want true")
	}
}

func TestBuildQueueEmbedShowsCurrentQueueAndAutoplay(t *testing.T) {
	gs := &guildState{
		isPlaying:       true,
		autoplayEnabled: true,
		lastPlayed: Track{
			Title:    "Current Song",
			Artist:   "Current Artist",
			Duration: "3:21",
			URL:      "https://www.youtube.com/watch?v=current",
		},
		songQueue: []Song{
			{track: Track{Title: "Next Song", Artist: "Next Artist", Duration: "4:00"}},
			{track: Track{Title: "Later Song", Artist: "Later Artist", Duration: "5:00"}},
		},
	}

	embed := buildQueueEmbed(gs, 1)

	if embed.Title != "Queue" {
		t.Fatalf("embed title = %q, want Queue", embed.Title)
	}
	if !strings.Contains(embed.Description, "Autoplay: On") {
		t.Fatalf("description = %q, want autoplay status", embed.Description)
	}
	if !strings.Contains(embed.Description, "Queued: 2") {
		t.Fatalf("description = %q, want queue count", embed.Description)
	}
	if len(embed.Fields) != 2 {
		t.Fatalf("fields length = %d, want 2", len(embed.Fields))
	}
	if !strings.Contains(embed.Fields[0].Value, "Current Song") {
		t.Fatalf("now playing field = %q, want current track", embed.Fields[0].Value)
	}
	if !strings.Contains(embed.Fields[1].Value, "1. Next Song") {
		t.Fatalf("up next field = %q, want first queued track", embed.Fields[1].Value)
	}
}

func TestBuildQueueEmbedPaginatesQueue(t *testing.T) {
	gs := &guildState{}
	for i := 1; i <= queuePageSize+1; i++ {
		gs.songQueue = append(gs.songQueue, Song{
			track: Track{Title: fmt.Sprintf("Song %02d", i)},
		})
	}

	embed := buildQueueEmbed(gs, 2)

	if embed.Footer == nil || embed.Footer.Text != "Page 2/2" {
		t.Fatalf("footer = %#v, want Page 2/2", embed.Footer)
	}
	if !strings.Contains(embed.Fields[1].Value, "11. Song 11") {
		t.Fatalf("up next field = %q, want second page track", embed.Fields[1].Value)
	}
	if strings.Contains(embed.Fields[1].Value, "1. Song 01") {
		t.Fatalf("up next field = %q, did not want first page track", embed.Fields[1].Value)
	}
}

func TestRemoveQueuedTrack(t *testing.T) {
	gs := &guildState{songQueue: []Song{
		{track: Track{Title: "One"}},
		{track: Track{Title: "Two"}},
		{track: Track{Title: "Three"}},
	}}

	removed, total, ok := gs.removeQueuedTrack(2)

	if !ok {
		t.Fatal("removeQueuedTrack() ok = false, want true")
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if removed.Title != "Two" {
		t.Fatalf("removed title = %q, want Two", removed.Title)
	}
	if got := queueTitles(gs.songQueue); got != "One,Three" {
		t.Fatalf("queue = %q, want One,Three", got)
	}
}

func TestRemoveQueuedTrackRejectsInvalidIndex(t *testing.T) {
	gs := &guildState{songQueue: []Song{{track: Track{Title: "One"}}}}

	_, total, ok := gs.removeQueuedTrack(2)

	if ok {
		t.Fatal("removeQueuedTrack() ok = true, want false")
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if got := queueTitles(gs.songQueue); got != "One" {
		t.Fatalf("queue = %q, want One", got)
	}
}

func TestMoveQueuedTrack(t *testing.T) {
	gs := &guildState{songQueue: []Song{
		{track: Track{Title: "One"}},
		{track: Track{Title: "Two"}},
		{track: Track{Title: "Three"}},
	}}

	moved, total, ok := gs.moveQueuedTrack(3, 1)

	if !ok {
		t.Fatal("moveQueuedTrack() ok = false, want true")
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if moved.Title != "Three" {
		t.Fatalf("moved title = %q, want Three", moved.Title)
	}
	if got := queueTitles(gs.songQueue); got != "Three,One,Two" {
		t.Fatalf("queue = %q, want Three,One,Two", got)
	}
}

func TestMoveQueuedTrackRejectsInvalidIndex(t *testing.T) {
	gs := &guildState{songQueue: []Song{
		{track: Track{Title: "One"}},
		{track: Track{Title: "Two"}},
	}}

	_, total, ok := gs.moveQueuedTrack(1, 3)

	if ok {
		t.Fatal("moveQueuedTrack() ok = true, want false")
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if got := queueTitles(gs.songQueue); got != "One,Two" {
		t.Fatalf("queue = %q, want One,Two", got)
	}
}

func TestClearQueuedTracks(t *testing.T) {
	gs := &guildState{
		isPlaying: true,
		songQueue: []Song{
			{track: Track{Title: "One"}},
			{track: Track{Title: "Two"}},
		},
	}

	n := gs.clearQueuedTracks()

	if n != 2 {
		t.Fatalf("cleared count = %d, want 2", n)
	}
	if len(gs.songQueue) != 0 {
		t.Fatalf("queue length = %d, want 0", len(gs.songQueue))
	}
	if !gs.isPlaying {
		t.Fatal("isPlaying = false, want true")
	}
}

func queueTitles(queue []Song) string {
	titles := make([]string, 0, len(queue))
	for _, song := range queue {
		titles = append(titles, song.track.Title)
	}
	return strings.Join(titles, ",")
}
