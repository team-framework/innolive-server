package vod

import (
	"errors"
	"io"
	"testing"

	"inno-live-server/internal/config"
)

type countedInput struct{ remaining, read int64 }

func (r *countedInput) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.read += n
	return int(n), nil
}

type countedOutput int64

func (w *countedOutput) Write(p []byte) (int, error) { *w += countedOutput(len(p)); return len(p), nil }

func TestUploadStreamingWorkerByteBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra int64
	}{{"exact", 0}, {"plus_one", 1}, {"unbounded_tail", 1 << 30}} {
		t.Run(tc.name, func(t *testing.T) {
			limit := config.VODWorkerMaxUploadBytes
			src := &countedInput{remaining: limit + tc.extra}
			var dst countedOutput
			n, err := copyUpload(&dst, src, limit)
			if n != limit || int64(dst) != limit {
				t.Fatalf("written=%d returned=%d", dst, n)
			}
			if tc.extra == 0 && err != nil {
				t.Fatal(err)
			}
			if tc.extra > 0 && !errors.Is(err, errUploadTooLarge) {
				t.Fatal(err)
			}
			if src.read != limit+min(tc.extra, 1) {
				t.Fatalf("read beyond overflow byte: %d", src.read)
			}
		})
	}
}
