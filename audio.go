package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// OGG/Opus demuxer
// ---------------------------------------------------------------------------
//
// ffmpeg writes an OGG container for both the Opus-copy and libopus-transcode
// pipelines. OGG pages have a fixed 27-byte header plus a variable-length
// segment table. The first two pages are the Opus identification and comment
// headers; subsequent pages carry Opus packets that are forwarded to Discord.
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

type oggReader struct {
	header      [27]byte
	segTable    [255]byte
	packetSizes [255]int
	packets     [255][]byte
	packetSlots int
	page        oggPage
}

// readOggPage reads and parses the next OGG page from r.
func readOggPage(r io.Reader) (*oggPage, error) {
	var reader oggReader
	return reader.readPage(r)
}

// readPage reads and parses the next OGG page from r, reusing parser scratch
// buffers across calls. Packet byte slices are still newly allocated because
// vc.OpusSend consumes them asynchronously.
func (or *oggReader) readPage(r io.Reader) (*oggPage, error) {
	// Fixed 27-byte header.
	header := or.header[:]
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 'O' || header[1] != 'g' || header[2] != 'g' || header[3] != 'S' {
		return nil, fmt.Errorf("ogg: bad capture pattern %q", header[0:4])
	}

	page := &or.page
	page.headerType = header[5]
	page.granulePos = binary.LittleEndian.Uint64(header[6:14])
	page.sequenceNum = binary.LittleEndian.Uint32(header[18:22])
	for i := 0; i < or.packetSlots; i++ {
		or.packets[i] = nil
	}
	page.packets = or.packets[:0]
	or.packetSlots = 0

	nSegs := int(header[26])
	segTable := or.segTable[:nSegs]
	if _, err := io.ReadFull(r, segTable); err != nil {
		return nil, fmt.Errorf("ogg: reading segment table: %w", err)
	}

	// The lacing table tells us each packet's size before we read the page body.
	// Reading packet-sized chunks avoids allocating a whole-page body and then
	// copying each packet out of it.
	packetCount := 0
	packetLen := 0
	for _, s := range segTable {
		packetLen += int(s)
		if s < 255 { // last segment of this packet
			or.packetSizes[packetCount] = packetLen
			packetCount++
			packetLen = 0
		}
	}
	if packetLen > 0 {
		// Continued packet with no terminator on this page (rare).
		or.packetSizes[packetCount] = packetLen
		packetCount++
	}

	for i := 0; i < packetCount; i++ {
		pkt := make([]byte, or.packetSizes[i])
		if _, err := io.ReadFull(r, pkt); err != nil {
			return nil, fmt.Errorf("ogg: reading page body: %w", err)
		}
		page.packets = append(page.packets, pkt)
	}
	or.packetSlots = packetCount

	return page, nil
}

// ---------------------------------------------------------------------------
// Audio streaming
// ---------------------------------------------------------------------------

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer {
	if max < 1 {
		max = processOutputTailMax
	}
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(p) >= b.max {
		if cap(b.buf) < b.max {
			b.buf = make([]byte, b.max)
		}
		b.buf = b.buf[:b.max]
		copy(b.buf, p[len(p)-b.max:])
		return len(p), nil
	}

	b.buf = append(b.buf, p...)
	if overflow := len(b.buf) - b.max; overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:b.max]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func processOutputTail(name string, stderr *tailBuffer) string {
	if stderr == nil {
		return ""
	}
	output := strings.TrimSpace(stderr.String())
	if output == "" {
		return ""
	}
	return name + " stderr: " + output
}

func streamProcessError(reason string, err error, dlStderr, ffStderr *tailBuffer) error {
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

func voiceConnectionSendState(vc *discordgo.VoiceConnection) string {
	if vc == nil {
		return "voice_state=nil"
	}
	vc.RLock()
	ready := vc.Ready
	guildID := vc.GuildID
	channelID := vc.ChannelID
	vc.RUnlock()

	queueLen, queueCap := 0, 0
	if vc.OpusSend != nil {
		queueLen = len(vc.OpusSend)
		queueCap = cap(vc.OpusSend)
	}
	return fmt.Sprintf("voice_state=ready:%t guild:%s channel:%s opus_queue:%d/%d",
		ready, guildID, channelID, queueLen, queueCap)
}

func sendOpusPacket(gs *guildState, vc *discordgo.VoiceConnection, pkt []byte, generation uint64) error {
	if vc == nil || vc.OpusSend == nil {
		return errors.New("voice connection is not ready to send audio")
	}
	if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
		return nil
	}
	select {
	case vc.OpusSend <- pkt:
		return nil
	default:
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
			return fmt.Errorf("%w (%s)", errOpusSendTimeout, voiceConnectionSendState(vc))
		case <-ticker.C:
		}
	}
}

// streamAudio pipes yt-dlp into ffmpeg and forwards Opus packets to Discord.
// It prefers remuxing an existing YouTube Opus stream and falls back to libopus
// transcoding only if the copy pipeline fails before sending audio packets.
func streamAudio(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string, generation uint64) error {
	packetsSent, err := streamAudioPipeline(gs, vc, dl, youtubeURL, generation, opusCopyPipeline)
	if err == nil || gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
		return err
	}
	if packetsSent > 0 {
		return err
	}
	log.Printf("stream pipeline fallback: from=%s to=%s url=%s err=%v",
		opusCopyPipeline.name, opusTranscodePipeline.name, youtubeURL, err)
	_, err = streamAudioPipeline(gs, vc, dl, youtubeURL, generation, opusTranscodePipeline)
	return err
}

func ytdlpCommandArgs(pipeline audioPipeline, youtubeURL string) []string {
	return []string{
		"--no-playlist",
		"--no-progress",
		"--no-update",
		"--remote-components", "ejs:github",
		// Avoid YouTube clients that intermittently return HTTP 403 media URLs.
		"--extractor-args", "youtube:player_client=default,web_embedded,-android_vr,-android_sdkless;player_js_version=actual",
		"-f", pipeline.ytdlpFormat,
		"-o", "-",
		youtubeURL,
	}
}

func streamAudioPipeline(gs *guildState, vc *discordgo.VoiceConnection, dl, youtubeURL string, generation uint64, pipeline audioPipeline) (int, error) {
	dlStderr := newTailBuffer(processOutputTailMax)
	ffStderr := newTailBuffer(processOutputTailMax)

	dlCmd := exec.Command(dl, ytdlpCommandArgs(pipeline, youtubeURL)...)
	dlCmd.Stderr = dlStderr

	ffCmd := exec.Command("ffmpeg", pipeline.ffmpegArgs...)
	ffCmd.Stderr = ffStderr

	dlStdout, err := dlCmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("%s yt-dlp stdout pipe: %w", pipeline.name, err)
	}
	ffCmd.Stdin = dlStdout

	ffStdout, err := ffCmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("%s ffmpeg stdout pipe: %w", pipeline.name, err)
	}

	if err := dlCmd.Start(); err != nil {
		return 0, streamProcessError(pipeline.name+" yt-dlp start", err, dlStderr, ffStderr)
	}
	if err := ffCmd.Start(); err != nil {
		_ = dlCmd.Process.Kill()
		return 0, streamProcessError(pipeline.name+" ffmpeg start", err, dlStderr, ffStderr)
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

	var ogg oggReader

	// Skip the two mandatory Opus header pages (identification + comment).
	// pageIndex 0 = OpusHead, pageIndex 1 = OpusTags.
	for skip := 0; skip < 2; skip++ {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			break
		}
		if _, err := ogg.readPage(ffStdout); err != nil {
			if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
				break
			}
			_ = dlCmd.Process.Kill()
			_ = ffCmd.Process.Kill()
			_, _ = dlCmd.Wait(), ffCmd.Wait()
			return 0, streamProcessError(fmt.Sprintf("%s ogg header page %d", pipeline.name, skip), err, dlStderr, ffStderr)
		}
	}

	// Stream audio pages until EOF, skip, or playback generation change.
	packetsSent := 0
	for {
		if gs.skipInterrupt.Load() || gs.playbackGeneration.Load() != generation {
			break
		}

		page, err := ogg.readPage(ffStdout)
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
			return packetsSent, streamProcessError(pipeline.name+" ogg demux", err, dlStderr, ffStderr)
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
					return packetsSent, err
				}
				if !gs.skipInterrupt.Load() && gs.playbackGeneration.Load() == generation {
					packetsSent++
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
		return packetsSent, nil
	}
	if dlErr != nil {
		return packetsSent, streamProcessError(pipeline.name+" yt-dlp exit", dlErr, dlStderr, ffStderr)
	}
	if ffErr != nil {
		return packetsSent, streamProcessError(pipeline.name+" ffmpeg exit", ffErr, dlStderr, ffStderr)
	}
	return packetsSent, nil
}
