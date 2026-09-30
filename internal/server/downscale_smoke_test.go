package server

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"testing"
)

func TestDownscaleForAISmoke(t *testing.T) {
	// 큰 2000x1500 이미지 -> 긴 변 640 이하로 돌아와야 한다
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
	// 작은 이미지는 그대로 통과한다
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
	// 압축률이 높은 8192x8192 이미지는 파일은 작지만 디코드하면 수백 MiB가 된다.
	// 디코드 전에 헤더에서 거절해야 한다.
	oversized := image.NewRGBA(image.Rect(0, 0, 8192, 8192))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, oversized, nil)
	if _, err := downscaleForAI(buf.Bytes()); !errors.Is(err, errImageTooLarge) {
		t.Fatalf("expected errImageTooLarge, got %v", err)
	}

	// 픽셀 예산 안인 넓고 얇은 이미지도 변 길이 한계는 넘는다.
	wide := image.NewRGBA(image.Rect(0, 0, 6000, 200))
	var wbuf bytes.Buffer
	_ = jpeg.Encode(&wbuf, wide, nil)
	if _, err := downscaleForAI(wbuf.Bytes()); !errors.Is(err, errImageTooLarge) {
		t.Fatalf("expected errImageTooLarge for wide image, got %v", err)
	}
}
