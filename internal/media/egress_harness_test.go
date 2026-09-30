//go:build egress_harness

// egress 검증 하네스. 단위 테스트가 아니다. 합성 JPEG 프레임으로 실제
// RTMPEgress(실제 ffmpeg 자식, 실제 FLV/RTMP 출력)를 돌려, 단계별 셸 검증(ffprobe,
// pgrep, SIGSTOP, RTMP 재연결)이 라이브 WebRTC 세션 없이 미디어 입력을 갖게 한다.
//
//	go test -tags egress_harness -run TestEgressHarnessStream ./internal/media -v \
//	    -timeout 20m  # 환경 변수 EGRESS_OUT, EGRESS_SECONDS, EGRESS_FPS
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

// EGRESS_WIDTH / EGRESS_HEIGHT는 합성 프레임 해상도다(기본 640x360, FHD 통과
// 검증에는 예: 1920x1080).
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

// harnessVideoSize는 EGRESS_VIDEO_SIZE를 그대로 전달해, 출력 해상도 고정을
// 켠 상태와 끈 상태를 같은 하네스로 비교할 수 있게 한다.
func harnessVideoSize() string {
	return os.Getenv("EGRESS_VIDEO_SIZE")
}

// harnessWireFormat은 EGRESS_WIRE(jpeg|raw)로 프레임 형식을 고른다. 서버의
// AI_FRAME_WIRE_FORMAT과 같다.
func harnessWireFormat() config.WireFormat {
	if os.Getenv("EGRESS_WIRE") == string(config.WireFormatRaw) {
		return config.WireFormatRaw
	}
	return config.WireFormatJPEG
}

// harnessRawYUV는 yuv420p 프레임 하나를 만든다. 밝기 그라데이션에 움직이는 밝은
// 상자, 중립 색차다.
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
	return NewRTMPEgress("ffmpeg", logger, metrics.New(), TranscoderOptions{WireFormat: harnessWireFormat()}, output, nil, false, 0, "", harnessVideoSize())
}

// TestEgressHarnessStream은 EGRESS_SECONDS 동안 실시간 속도로 합성 프레임을 넣고
// 최대 Enqueue 지연을 알린다(역압 증거: ffmpeg 자식을 SIGSTOP해도 아주 작아야 한다).
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

// TestEgressHarnessRestartLoop는 egress를 여러 번 올렸다 내린다. 이후 셸 검증이
// 남은 ffmpeg 프로세스가 없는지 확인한다.
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
