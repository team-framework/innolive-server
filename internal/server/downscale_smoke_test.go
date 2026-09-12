package server

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"testing"
)

func TestDownscaleForAISmoke(t *testing.T) {
	// large 2000x1500 image -> must come back <=640 long edge
	big := image.NewRGBA(image.Rect(0, 0, 2000, 1500))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, big, nil)
	out, err := downscaleForAI(buf.Bytes())
	if err != nil {
		t.Fatalf("downscale: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Width > 640 || cfg.Height > 640 {
		t.Fatalf("not downscaled: %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width != 640 {
		t.Fatalf("expected long edge 640, got %dx%d", cfg.Width, cfg.Height)
	}
	// small image passes through unchanged
	small := image.NewRGBA(image.Rect(0, 0, 300, 200))
	var sbuf bytes.Buffer
	_ = jpeg.Encode(&sbuf, small, nil)
	got, err := downscaleForAI(sbuf.Bytes())
	if err != nil {
		t.Fatalf("downscale small: %v", err)
	}
	if !bytes.Equal(got, sbuf.Bytes()) {
		t.Fatalf("small image should pass through unchanged")
	}
	t.Logf("downscaled 2000x1500 -> %dx%d", cfg.Width, cfg.Height)
}

func TestDownscaleForAIRejectsOversized(t *testing.T) {
	// A highly compressible 8192x8192 image has a tiny file size but would
	// decode into hundreds of MiB. Its header must be rejected before decode.
	oversized := image.NewRGBA(image.Rect(0, 0, 8192, 8192))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, oversized, nil)
	if _, err := downscaleForAI(buf.Bytes()); !errors.Is(err, errImageTooLarge) {
		t.Fatalf("expected errImageTooLarge, got %v", err)
	}

	// A wide-but-thin image under the pixel budget still exceeds the edge limit.
	wide := image.NewRGBA(image.Rect(0, 0, 6000, 200))
	var wbuf bytes.Buffer
	_ = jpeg.Encode(&wbuf, wide, nil)
	if _, err := downscaleForAI(wbuf.Bytes()); !errors.Is(err, errImageTooLarge) {
		t.Fatalf("expected errImageTooLarge for wide image, got %v", err)
	}
}
