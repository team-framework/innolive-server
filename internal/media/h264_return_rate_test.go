package media

import (
	"context"
	"fmt"
	"inno-live-server/internal/config"
	"math/rand"
	"os/exec"
	"testing"
	"time"
)

// TestH264ReturnRateBoundsHighDetailFrames는 상한 없는 출력의 키프레임 버스트를
// 막는 VBV(#461)가 1초 버퍼(#478)에서도 지켜지는지 최악 입력(무작위 화소)으로
// 확인한다. 한 프레임은 버퍼(bitrate×1초)를, 1초 합계는 maxrate+버퍼를 넘을 수 없다.
func TestH264ReturnRateBoundsHighDetailFrames(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	for _, tc := range []struct {
		width, height uint16
		bitsPerSecond int
	}{
		{1280, 720, 2_000_000},
		{1080, 1920, 4_000_000},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			transcoder := NewFFmpegTranscoder(path, testLogger(), nil, TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatRaw, EncoderThreads: 1})
			input := make(chan frame, 4)
			output := make(chan frame, 4)
			done := make(chan error, 1)
			go func() { done <- transcoder.EncodeStream(ctx, input, output); close(output) }()
			go func() {
				defer close(input)
				rng := rand.New(rand.NewSource(461))
				luma := int(tc.width) * int(tc.height)
				for i := 0; i < 30; i++ {
					data := make([]byte, rawFrameSize(tc.width, tc.height))
					rng.Read(data[:luma])
					for k := luma; k < len(data); k++ {
						data[k] = 128
					}
					select {
					case input <- frame{data: data, width: tc.width, height: tc.height, timestamp: uint32(i * 3000)}:
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
			// 액세스 유닛에는 SPS/PPS·AUD 헤더가 붙으므로 약간의 여유를 둔다.
			buffer := tc.bitsPerSecond / 8
			if largest > buffer+1024 || total > 2*buffer+1024 {
				t.Fatalf("return stream exceeds VBV budget: largest=%d bytes (buffer %d), one-second total=%d bytes (limit %d)", largest, buffer, total, 2*buffer)
			}
			t.Logf("30 high-detail frames: largest=%d bytes, total=%d bytes", largest, total)
		})
	}
}
