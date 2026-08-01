//go:build egress_harness

// Egress verification harness. Not a unit test: it drives the real RTMPEgress
// (real ffmpeg child, real FLV/RTMP output) with synthetic JPEG frames so the
// staged shell verifications (ffprobe, pgrep, SIGSTOP, RTMP reconnect) have a
// media source without a live WebRTC session.
//
//	go test -tags egress_harness -run TestEgressHarnessStream ./internal/media -v \
//	    -timeout 20m  # EGRESS_OUT, EGRESS_SECONDS, EGRESS_FPS env knobs
package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// EGRESS_WIDTH / EGRESS_HEIGHT select the synthetic frame resolution
// (default 640x360; e.g. 1920x1080 for the FHD pass-through verification).
var (
	harnessWidth  = harnessDimension("EGRESS_WIDTH", 640)
	harnessHeight = harnessDimension("EGRESS_HEIGHT", 360)
)

func harnessDimension(key string, fallback int) int {
	parsed, err := strconv.Atoi(os.Getenv(key))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func harnessEnvInt(t *testing.T, key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("invalid %s: %v", key, err)
	}
	return parsed
}

func harnessJPEG(t *testing.T, index int) []byte {
	canvas := image.NewRGBA(image.Rect(0, 0, harnessWidth, harnessHeight))
	for y := 0; y < harnessHeight; y++ {
		for x := 0; x < harnessWidth; x++ {
			canvas.Set(x, y, color.RGBA{R: uint8(x / 3), G: uint8(y / 2), B: uint8(index * 2), A: 255})
		}
	}
	box := (index * 4) % (harnessWidth - 40)
	for y := 100; y < 180; y++ {
		for x := box; x < box+40; x++ {
			canvas.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode harness frame: %v", err)
	}
	return encoded.Bytes()
}

// harnessWireFormat selects the frame format via EGRESS_WIRE (jpeg|raw),
// mirroring the server's AI_FRAME_WIRE_FORMAT.
func harnessWireFormat() config.WireFormat {
	if os.Getenv("EGRESS_WIRE") == string(config.WireFormatRaw) {
		return config.WireFormatRaw
	}
	return config.WireFormatJPEG
}

// harnessRawYUV builds one yuv420p frame: luma gradient plus a moving bright
// box, neutral chroma.
func harnessRawYUV(index int) []byte {
	size := rawFrameSize(uint16(harnessWidth), uint16(harnessHeight))
	data := make([]byte, size)
	for y := 0; y < harnessHeight; y++ {
		for x := 0; x < harnessWidth; x++ {
			data[y*harnessWidth+x] = uint8((x+y+index*4)%220 + 16)
		}
	}
	box := (index * 4) % (harnessWidth - 40)
	for y := 100; y < 180; y++ {
		for x := box; x < box+40; x++ {
			data[y*harnessWidth+x] = 235
		}
	}
	for i := harnessWidth * harnessHeight; i < size; i++ {
		data[i] = 128
	}
	return data
}

func harnessFrameData(t *testing.T, index int, wire config.WireFormat) []byte {
	if wire == config.WireFormatRaw {
		return harnessRawYUV(index)
	}
	return harnessJPEG(t, index)
}

func newHarnessEgress(t *testing.T, output string) *RTMPEgress {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewRTMPEgress("ffmpeg", logger, metrics.New(), TranscoderOptions{WireFormat: harnessWireFormat()}, output, nil, false, 0, "")
}

// TestEgressHarnessStream feeds synthetic frames at real-time pace for
// EGRESS_SECONDS and reports the maximum Enqueue latency (backpressure
// evidence: it must stay tiny even when the ffmpeg child is SIGSTOPped).
func TestEgressHarnessStream(t *testing.T) {
	output := os.Getenv("EGRESS_OUT")
	if output == "" {
		output = "out.flv"
	}
	fps := harnessEnvInt(t, "EGRESS_FPS", 30)
	seconds := harnessEnvInt(t, "EGRESS_SECONDS", 10)

	egress := newHarnessEgress(t, output)
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		egress.Run(ctx)
	}()

	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()
	deadline := time.After(time.Duration(seconds) * time.Second)
	timestamp := uint32(90000)
	step := uint32(videoClockRate / fps)
	var maxEnqueue time.Duration
	startedAt := time.Now()
	index := 0
feed:
	for {
		select {
		case <-deadline:
			break feed
		case <-ticker.C:
			item := frame{
				data:      harnessFrameData(t, index, harnessWireFormat()),
				timestamp: timestamp,
				stageAt:   time.Now(),
				width:     uint16(harnessWidth),
				height:    uint16(harnessHeight),
			}
			enqueuedAt := time.Now()
			egress.Enqueue(item)
			if elapsed := time.Since(enqueuedAt); elapsed > maxEnqueue {
				maxEnqueue = elapsed
			}
			timestamp += step
			index++
		}
	}
	elapsed := time.Since(startedAt)
	cancel()
	done.Wait()
	t.Logf("fed %d frames over %s, max Enqueue latency %s", index, elapsed, maxEnqueue)
}

// TestEgressHarnessRestartLoop spins the egress up and down repeatedly; the
// shell verification afterwards asserts no ffmpeg process survived.
func TestEgressHarnessRestartLoop(t *testing.T) {
	iterations := harnessEnvInt(t, "EGRESS_ITERATIONS", 30)
	data := harnessFrameData(t, 0, harnessWireFormat())
	directory := t.TempDir()
	for i := 0; i < iterations; i++ {
		egress := newHarnessEgress(t, directory+"/loop-"+strconv.Itoa(i)+".flv")
		ctx, cancel := context.WithCancel(context.Background())
		var done sync.WaitGroup
		done.Add(1)
		go func() {
			defer done.Done()
			egress.Run(ctx)
		}()
		timestamp := uint32(90000)
		for sent := 0; sent < egressMeasureFrames+10; sent++ {
			egress.Enqueue(frame{data: data, timestamp: timestamp, width: uint16(harnessWidth), height: uint16(harnessHeight)})
			timestamp += 3000
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
		done.Wait()
	}
	t.Logf("completed %d start/stop cycles", iterations)
}
