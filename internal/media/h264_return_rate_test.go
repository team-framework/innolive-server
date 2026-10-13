package media

import (
	"context"
	"inno-live-server/internal/config"
	"math/rand"
	"os/exec"
	"testing"
	"time"
)

func TestH264ReturnRateBoundsHighDetailFrames(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transcoder := NewFFmpegTranscoder(path, testLogger(), nil, TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatRaw, EncoderThreads: 1})
	input := make(chan frame, 4)
	output := make(chan frame, 4)
	done := make(chan error, 1)
	go func() { done <- transcoder.EncodeStream(ctx, input, output); close(output) }()
	go func() {
		defer close(input)
		rng := rand.New(rand.NewSource(461))
		for i := 0; i < 30; i++ {
			data := make([]byte, rawFrameSize(1280, 720))
			rng.Read(data[:1280*720])
			for k := 1280 * 720; k < len(data); k++ {
				data[k] = 128
			}
			select {
			case input <- frame{data: data, width: 1280, height: 720, timestamp: uint32(i * 3000)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	count, total, largest := 0, 0, 0
	for item := range output {
		count++
		total += len(item.data)
		if len(item.data) > largest {
			largest = len(item.data)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if count != 30 {
		t.Fatalf("encoded frames=%d, want 30", count)
	}
	if largest > 50000 || total > 312500 {
		t.Fatalf("return stream exceeds bounded preview budget: largest=%d bytes, one-second total=%d bytes", largest, total)
	}
	t.Logf("30 high-detail frames: largest=%d bytes, total=%d bytes", largest, total)
}
