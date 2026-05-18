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
		disconnectTimer:      time.AfterFunc(time.Hour, func() {}),
		nowPlayingChannelID:  "text",
		nowPlayingMessageID:  "message",
		nowPlayingControlID:  "control",
		nowPlayingControlSeq: 10,
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
	if gs.nowPlayingChannelID != "" || gs.nowPlayingMessageID != "" || gs.nowPlayingControlID != "" {
		t.Fatalf("now playing controls = %q/%q/%q, want cleared", gs.nowPlayingChannelID, gs.nowPlayingMessageID, gs.nowPlayingControlID)
	}
}

func TestIdleDisconnectTimeoutIsFiveMinutes(t *testing.T) {
	if idleDisconnectTimeout != 5*time.Minute {
		t.Fatalf("idleDisconnectTimeout = %s, want 5m0s", idleDisconnectTimeout)
	}
}

func TestBuildNowPlayingComponentsAddsControlButtons(t *testing.T) {
	components := buildNowPlayingComponents("42")
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
		{label: "Skip", customID: nowPlayingCustomID(nowPlayingButtonSkip, "42"), style: discordgo.PrimaryButton},
		{label: "Stop", customID: nowPlayingCustomID(nowPlayingButtonStop, "42"), style: discordgo.DangerButton},
		{label: "Queue", customID: nowPlayingCustomID(nowPlayingButtonQueue, "42"), style: discordgo.SecondaryButton},
		{label: "Autoplay", customID: nowPlayingCustomID(nowPlayingButtonAutoplay, "42"), style: discordgo.SecondaryButton},
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

func TestActiveNowPlayingActionRejectsStaleControls(t *testing.T) {
	gs := &guildState{nowPlayingControlID: "current"}

	action, ok := gs.activeNowPlayingAction(nowPlayingCustomID(nowPlayingButtonSkip, "current"))
	if !ok {
		t.Fatal("activeNowPlayingAction() ok = false, want true")
	}
	if action != nowPlayingButtonSkip {
		t.Fatalf("activeNowPlayingAction() action = %q, want %q", action, nowPlayingButtonSkip)
	}

	if _, ok := gs.activeNowPlayingAction(nowPlayingCustomID(nowPlayingButtonSkip, "old")); ok {
		t.Fatal("activeNowPlayingAction() ok = true, want false for stale control")
	}
	if _, ok := gs.activeNowPlayingAction(nowPlayingButtonSkip); ok {
		t.Fatal("activeNowPlayingAction() ok = true, want false for legacy unscoped control")
	}
}

func TestActivateNowPlayingControlsReturnsPreviousMessage(t *testing.T) {
	gs := &guildState{
		nowPlayingChannelID:  "old-channel",
		nowPlayingMessageID:  "old-message",
		nowPlayingControlID:  "old-control",
		nowPlayingControlSeq: 7,
	}

	controlID, previous := gs.activateNowPlayingControls("new-channel")

	if controlID != "8" {
		t.Fatalf("controlID = %q, want 8", controlID)
	}
	if previous.channelID != "old-channel" || previous.messageID != "old-message" {
		t.Fatalf("previous = %#v, want old-channel/old-message", previous)
	}
	if gs.nowPlayingChannelID != "new-channel" || gs.nowPlayingMessageID != "" || gs.nowPlayingControlID != "8" {
		t.Fatalf("active controls = %q/%q/%q, want new-channel//8", gs.nowPlayingChannelID, gs.nowPlayingMessageID, gs.nowPlayingControlID)
	}
}

func TestVoiceChannelHasListenersIgnoresOnlyBot(t *testing.T) {
	oldBotID := BotID
	BotID = "bot"
	t.Cleanup(func() { BotID = oldBotID })

	g := &discordgo.Guild{
		VoiceStates: []*discordgo.VoiceState{
			{UserID: "bot", ChannelID: "voice"},
		},
	}

	if voiceChannelHasListeners(g, "voice") {
		t.Fatal("voiceChannelHasListeners() = true, want false when only the bot remains")
	}

	g.VoiceStates = append(g.VoiceStates, &discordgo.VoiceState{UserID: "listener", ChannelID: "voice"})
	if !voiceChannelHasListeners(g, "voice") {
		t.Fatal("voiceChannelHasListeners() = false, want true with another user present")
	}
}

func TestBotVoiceChannelIDFallsBackToState(t *testing.T) {
	oldBotID := BotID
	BotID = "bot"
	t.Cleanup(func() { BotID = oldBotID })

	state := discordgo.NewState()
	if err := state.GuildAdd(&discordgo.Guild{
		ID: "guild",
		VoiceStates: []*discordgo.VoiceState{
			{UserID: "bot", ChannelID: "voice"},
		},
	}); err != nil {
		t.Fatalf("GuildAdd() error = %v", err)
	}
	s := &discordgo.Session{State: state}

	if got := botVoiceChannelID(s, "guild"); got != "voice" {
		t.Fatalf("botVoiceChannelID() = %q, want voice", got)
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

func TestGuildActionsRunInQueuedOrder(t *testing.T) {
	gs := newGuildState()
	defer close(gs.actionQueue)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan int, 2)

	if !gs.enqueueAction(func() {
		close(started)
		<-release
		done <- 1
	}) {
		t.Fatal("first enqueueAction() = false, want true")
	}
	if !gs.enqueueAction(func() {
		done <- 2
	}) {
		t.Fatal("second enqueueAction() = false, want true")
	}

	<-started
	select {
	case got := <-done:
		t.Fatalf("action %d completed before the first action released", got)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	for want := 1; want <= 2; want++ {
		select {
		case got := <-done:
			if got != want {
				t.Fatalf("completed action = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for action %d", want)
		}
	}
}

func TestGuildActionsRunConcurrentlyAcrossGuilds(t *testing.T) {
	gs1 := newGuildState()
	gs2 := newGuildState()
	defer close(gs1.actionQueue)
	defer close(gs2.actionQueue)

	started1 := make(chan struct{})
	finished2 := make(chan struct{})
	release1 := make(chan struct{})

	if !gs1.enqueueAction(func() {
		close(started1)
		<-release1
	}) {
		t.Fatal("first guild enqueueAction() = false, want true")
	}
	if !gs2.enqueueAction(func() {
		close(finished2)
	}) {
		t.Fatal("second guild enqueueAction() = false, want true")
	}

	select {
	case <-started1:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first guild action to start")
	}
	select {
	case <-finished2:
	case <-time.After(time.Second):
		t.Fatal("second guild action was blocked by first guild action")
	}
	close(release1)
}

func TestConfigureStreamLimiterDefaultsToFive(t *testing.T) {
	t.Cleanup(func() { configureStreamLimiter(defaultMaxStreams) })

	configureStreamLimiter(0)

	if maxConcurrentStreams != defaultMaxStreams {
		t.Fatalf("maxConcurrentStreams = %d, want %d", maxConcurrentStreams, defaultMaxStreams)
	}
	if cap(streamSlotChannel()) != defaultMaxStreams {
		t.Fatalf("stream slot capacity = %d, want %d", cap(streamSlotChannel()), defaultMaxStreams)
	}
}

func TestStreamLimiterWaitsForCapacity(t *testing.T) {
	t.Cleanup(func() { configureStreamLimiter(defaultMaxStreams) })
	configureStreamLimiter(1)

	gs1 := &guildState{}
	release1, _, ok := acquireStreamSlot(gs1, gs1.playbackGeneration.Load())
	if !ok {
		t.Fatal("first acquireStreamSlot() = false, want true")
	}

	gs2 := &guildState{}
	done := make(chan bool, 1)
	go func() {
		release2, _, ok := acquireStreamSlot(gs2, gs2.playbackGeneration.Load())
		if ok {
			release2()
		}
		done <- ok
	}()

	select {
	case ok := <-done:
		t.Fatalf("second acquireStreamSlot() completed early with %v", ok)
	case <-time.After(50 * time.Millisecond):
	}

	release1()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("second acquireStreamSlot() = false, want true after capacity frees")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second stream slot")
	}
}

func TestStreamLimiterCancelsWhenPlaybackIsInterrupted(t *testing.T) {
	t.Cleanup(func() { configureStreamLimiter(defaultMaxStreams) })
	configureStreamLimiter(1)

	gs1 := &guildState{}
	release1, _, ok := acquireStreamSlot(gs1, gs1.playbackGeneration.Load())
	if !ok {
		t.Fatal("first acquireStreamSlot() = false, want true")
	}
	defer release1()

	gs2 := &guildState{}
	done := make(chan bool, 1)
	go func() {
		_, _, ok := acquireStreamSlot(gs2, gs2.playbackGeneration.Load())
		done <- ok
	}()

	select {
	case ok := <-done:
		t.Fatalf("second acquireStreamSlot() completed early with %v", ok)
	case <-time.After(50 * time.Millisecond):
	}

	gs2.skipInterrupt.Store(true)

	select {
	case ok := <-done:
		if ok {
			t.Fatal("second acquireStreamSlot() = true, want false after interrupt")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for interrupted stream slot")
	}
}

func TestOpusCopyPipelineAvoidsTranscoding(t *testing.T) {
	if opusCopyPipeline.ytdlpFormat != "bestaudio[acodec=opus]" {
		t.Fatalf("copy ytdlp format = %q, want opus-only format", opusCopyPipeline.ytdlpFormat)
	}

	args := strings.Join(opusCopyPipeline.ffmpegArgs, " ")
	if !strings.Contains(args, "-nostdin -hide_banner -loglevel warning") {
		t.Fatalf("copy ffmpeg args = %q, want quiet noninteractive flags", args)
	}
	if !strings.Contains(args, "-c:a copy") {
		t.Fatalf("copy ffmpeg args = %q, want -c:a copy", args)
	}
	if strings.Contains(args, "libopus") {
		t.Fatalf("copy ffmpeg args = %q, should not transcode with libopus", args)
	}
}

func TestOpusTranscodePipelineKeepsFallbackEncoder(t *testing.T) {
	if opusTranscodePipeline.ytdlpFormat != "bestaudio" {
		t.Fatalf("transcode ytdlp format = %q, want bestaudio fallback", opusTranscodePipeline.ytdlpFormat)
	}

	args := strings.Join(opusTranscodePipeline.ffmpegArgs, " ")
	if !strings.Contains(args, "-nostdin -hide_banner -loglevel warning") {
		t.Fatalf("transcode ffmpeg args = %q, want quiet noninteractive flags", args)
	}
	if !strings.Contains(args, "-c:a libopus") {
		t.Fatalf("transcode ffmpeg args = %q, want libopus fallback", args)
	}
}

func TestYtdlpCommandArgsDisableProgress(t *testing.T) {
	args := strings.Join(ytdlpCommandArgs(opusCopyPipeline, "https://example.test/video"), " ")
	if !strings.Contains(args, "--no-progress") {
		t.Fatalf("yt-dlp args = %q, want --no-progress", args)
	}
	if !strings.Contains(args, "-f bestaudio[acodec=opus]") {
		t.Fatalf("yt-dlp args = %q, want copy pipeline format", args)
	}
}

func TestTailBufferKeepsOnlyRecentBytes(t *testing.T) {
	buf := newTailBuffer(5)
	if n, err := buf.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first Write() = %d, %v; want 3, nil", n, err)
	}
	if n, err := buf.Write([]byte("defgh")); err != nil || n != 5 {
		t.Fatalf("second Write() = %d, %v; want 5, nil", n, err)
	}
	if got := buf.String(); got != "defgh" {
		t.Fatalf("tail buffer = %q, want defgh", got)
	}

	if n, err := buf.Write([]byte("ijklmnop")); err != nil || n != 8 {
		t.Fatalf("large Write() = %d, %v; want 8, nil", n, err)
	}
	if got := buf.String(); got != "lmnop" {
		t.Fatalf("tail buffer = %q, want lmnop", got)
	}
}

func TestProcessOutputTailLabelsTrimmedTail(t *testing.T) {
	buf := newTailBuffer(20)
	_, _ = buf.Write([]byte("  useful error  \n"))

	if got := processOutputTail("ffmpeg", buf); got != "ffmpeg stderr: useful error" {
		t.Fatalf("processOutputTail() = %q, want labelled trimmed stderr", got)
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

func TestDisableAutoplayAfterPlaybackError(t *testing.T) {
	gs := &guildState{autoplayEnabled: true}
	generation := gs.playbackGeneration.Load()

	if !gs.disableAutoplayAfterPlaybackError(generation) {
		t.Fatal("disableAutoplayAfterPlaybackError() = false, want true")
	}
	if gs.autoplayEnabled {
		t.Fatal("autoplayEnabled = true, want false")
	}
	if gs.disableAutoplayAfterPlaybackError(generation) {
		t.Fatal("second disableAutoplayAfterPlaybackError() = true, want false")
	}
}

func TestDisableAutoplayAfterPlaybackErrorIgnoresStaleGeneration(t *testing.T) {
	gs := &guildState{autoplayEnabled: true}
	generation := gs.playbackGeneration.Load()
	gs.playbackGeneration.Add(1)

	if gs.disableAutoplayAfterPlaybackError(generation) {
		t.Fatal("disableAutoplayAfterPlaybackError() = true, want false for stale generation")
	}
	if !gs.autoplayEnabled {
		t.Fatal("autoplayEnabled = false, want true for stale generation")
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

func TestSendOpusPacketSendsOnReadyChannel(t *testing.T) {
	gs := &guildState{}
	vc := &discordgo.VoiceConnection{OpusSend: make(chan []byte, 1)}
	generation := gs.playbackGeneration.Load()
	pkt := []byte{1, 2, 3}

	if err := sendOpusPacket(gs, vc, pkt, generation); err != nil {
		t.Fatalf("sendOpusPacket() error = %v, want nil", err)
	}

	select {
	case got := <-vc.OpusSend:
		if string(got) != string(pkt) {
			t.Fatalf("packet = %v, want %v", got, pkt)
		}
	default:
		t.Fatal("sendOpusPacket did not send to ready channel")
	}
}

func BenchmarkSendOpusPacketFastPath(b *testing.B) {
	gs := &guildState{}
	vc := &discordgo.VoiceConnection{OpusSend: make(chan []byte, 1)}
	generation := gs.playbackGeneration.Load()
	pkt := []byte{1, 2, 3}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := sendOpusPacket(gs, vc, pkt, generation); err != nil {
			b.Fatal(err)
		}
		<-vc.OpusSend
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
