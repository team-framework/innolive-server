package vod

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFFProbeAcceptsHEVCAndPCMMov(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hevc := filepath.Join(dir, "hevc.mov")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=320x240:d=1",
		"-c:v", "libx265", "-tag:v", "hvc1", "-pix_fmt", "yuv420p", "-an", "-t", "1", hevc)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("libx265 unavailable: %v %s", err, b)
	}
	if _, err := (FFProbe{}).Probe(ctx, hevc); err != nil {
		t.Fatalf("hevc mov: %v", err)
	}
	pcm := filepath.Join(dir, "pcm.mov")
	cmd = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=green:s=320x240:d=1",
		"-f", "lavfi", "-i", "sine=f=440:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "pcm_s16le", "-shortest", "-t", "1", pcm)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pcm fixture: %v %s", err, b)
	}
	if _, err := (FFProbe{}).Probe(ctx, pcm); err != nil {
		t.Fatalf("pcm mov: %v", err)
	}
}
