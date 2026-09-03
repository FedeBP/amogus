package main

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
	defaultMemWarnInterval = 30 * time.Second
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
	processMemLogEvery   time.Duration
	processMemWarnRSS    uint64
	processMemLogVerbose bool
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

func parseOptionalPositiveDuration(value string) (time.Duration, error) {
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

func parsePlaybackMemLogInterval(value string) (time.Duration, error) {
	return parseOptionalPositiveDuration(value)
}

func parseOptionalPositiveMegabytes(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	mb, err := strconv.ParseUint(value, 10, 64)
	if err != nil || mb < 1 {
		return 0, fmt.Errorf("megabytes must be positive")
	}
	return mb * 1024 * 1024, nil
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

func safeRun(context string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in %s: %v\n%s", context, r, debug.Stack())
		}
	}()
	fn()
}

func safeGo(context string, fn func()) {
	go safeRun(context, fn)
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

func formatMemSnapshot(prefix, event string, s playbackMemSnapshot) string {
	active, capacity := streamSlotStats()
	fields := fmt.Sprintf(
		"%s mem: event=%s active=%d capacity=%d goroutines=%d heap_alloc=%d heap_sys=%d heap_idle=%d heap_released=%d stack_inuse=%d next_gc=%d sys=%d num_gc=%d",
		prefix,
		event,
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
	return fields
}

func startProcessMemMonitor() func() {
	interval := processMemLogEvery
	warnRSS := processMemWarnRSS
	if interval <= 0 && warnRSS > 0 {
		interval = defaultMemWarnInterval
	}
	if interval <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	warned := false

	safeGo("process memory monitor", func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s := capturePlaybackMemSnapshot()
				if processMemLogVerbose {
					log.Print(formatMemSnapshot("process", "tick", s))
				}
				if warnRSS > 0 && s.hasRSS {
					if s.rss >= warnRSS && !warned {
						log.Printf("%s warn_rss=%d", formatMemSnapshot("process", "rss_warning", s), warnRSS)
						warned = true
					} else if warned && s.rss < warnRSS*9/10 {
						warned = false
					}
				}
			case <-stop:
				return
			}
		}
	})

	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
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
	safeGo("playback memory logger", func() {
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
	})

	return func() {
		once.Do(func() {
			close(stop)
			<-done
			logPlaybackMemSnapshot("end", streamID, guildID, time.Since(started))
		})
	}
}
