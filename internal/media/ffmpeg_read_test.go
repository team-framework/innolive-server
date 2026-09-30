package media

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReadJPEGExtractsFramesAcrossBufferWindows(t *testing.T) {
	first := append([]byte{0xff, 0xd8}, bytes.Repeat([]byte{0xab}, 40)...)
	first = append(first, 0xff, 0x00, 0x11, 0xff, 0xd9)
	second := []byte{0xff, 0xd8, 0x01, 0x02, 0xff, 0xd9}
	stream := append(append([]byte(nil), first...), second...)

	// 16바이트 리더로 탐색이 버퍼 창 여러 개를 훑게 한다.
	reader := bufio.NewReaderSize(bytes.NewReader(stream), 16)
	got, err := readJPEG(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("first frame = %v, want %v", got, first)
	}
	got, err = readJPEG(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("second frame = %v, want %v", got, second)
	}
}

func TestReadJPEGHandlesEOISplitAcrossWindows(t *testing.T) {
	// 0xFF는 첫 16바이트 창의 끝에, 0xD9는 다음 창의 시작에 있어 창 경계 분기를
	// 탄다.
	frame := append([]byte{0xff, 0xd8}, bytes.Repeat([]byte{0x00}, 13)...)
	frame = append(frame, 0xff, 0xd9)
	trailer := []byte{0xff, 0xd8, 0xff, 0xd9}
	stream := append(append([]byte(nil), frame...), trailer...)

	reader := bufio.NewReaderSize(bytes.NewReader(stream), 16)
	got, err := readJPEG(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("frame = %v, want %v", got, frame)
	}
	got, err = readJPEG(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, trailer) {
		t.Fatalf("trailer frame = %v, want %v", got, trailer)
	}
}

func TestReadJPEGRejectsNonJPEGStream(t *testing.T) {
	reader := bufio.NewReaderSize(bytes.NewReader([]byte{0x00, 0x01, 0x02}), 16)
	if _, err := readJPEG(reader); err == nil {
		t.Fatal("readJPEG() should reject a stream without a JPEG SOI marker")
	}
}

func TestReadRawFrameReadsExactFrameSize(t *testing.T) {
	payload := bytes.Repeat([]byte{0x42}, rawFrameSize(4, 4)+3)
	reader := bufio.NewReader(bytes.NewReader(payload))
	got, err := readRawFrame(reader, rawFrameSize(4, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != rawFrameSize(4, 4) {
		t.Fatalf("raw frame size = %d, want %d", len(got), rawFrameSize(4, 4))
	}
}

func TestRawFrameSizeRoundsOddDimensions(t *testing.T) {
	if got, want := rawFrameSize(2, 2), 6; got != want {
		t.Fatalf("rawFrameSize(2,2) = %d, want %d", got, want)
	}
	if got, want := rawFrameSize(3, 3), 17; got != want {
		t.Fatalf("rawFrameSize(3,3) = %d, want %d", got, want)
	}
}

func TestEncodeStreamDefersSpawnUntilFirstFrame(t *testing.T) {
	var spawns atomic.Int32
	transcoder := NewFFmpegTranscoder("cat", testLogger(), nil, TranscoderOptions{})
	transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		spawns.Add(1)
		return exec.CommandContext(ctx, "cat")
	}

	input := make(chan frame, 1)
	output := make(chan frame, 4)
	result := make(chan error, 1)
	go func() { result <- transcoder.EncodeStream(context.Background(), input, output) }()

	time.Sleep(100 * time.Millisecond)
	if got := spawns.Load(); got != 0 {
		t.Fatalf("encoder spawned %d processes before any frame arrived, want 0", got)
	}

	input <- frame{data: []byte{0xff, 0xd8, 0xff, 0xd9}, width: 4, height: 4}
	close(input)

	select {
	case err := <-result:
		// cat은 FFmpeg가 아니라 IVF 헤더 확인은 실패해야 한다 — 이 테스트가 보는 것은
		// 프로세스 생성 시점뿐이다.
		if err == nil {
			t.Fatal("EncodeStream() with a fake encoder should return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EncodeStream() did not finish")
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("encoder spawn count = %d, want 1", got)
	}
}
