package server

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

func TestDownscaleForAISmoke(t *testing.T) {
	// large 2000x1500 image -> must come back <=640 long edge
	big := image.NewRGBA(image.Rect(0, 0, 2000, 1500))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, big, nil)
	out := downscaleForAI(buf.Bytes())
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
	if got := downscaleForAI(sbuf.Bytes()); !bytes.Equal(got, sbuf.Bytes()) {
		t.Fatalf("small image should pass through unchanged")
	}
	t.Logf("downscaled 2000x1500 -> %dx%d", cfg.Width, cfg.Height)
}
