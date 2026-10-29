package vod

import (
	"errors"
	"testing"
)

func TestEvaluateProbeStreamsIPhoneLayout(t *testing.T) {
	format := "mov,mp4,m4a,3gp,3g2,mj2"
	t.Run("hevc_aac_mjpeg_thumbnail", func(t *testing.T) {
		m, err := evaluateProbeStreams(format, "0.77", []probeStream{
			{CodecType: "video", CodecName: "hevc", Width: 1080, Height: 1920, NBFrames: "23"},
			{CodecType: "audio", CodecName: "aac"},
			{CodecType: "video", CodecName: "mjpeg", Width: 320, Height: 180},
			{CodecType: "data", CodecName: "mebx"},
			{CodecType: "data", CodecName: "mebx"},
		})
		if err != nil || m.Duration != 0.77 || m.Frames != 23 {
			t.Fatalf("got %+v err=%v", m, err)
		}
	})
	t.Run("hevc_aac_apac_spatial", func(t *testing.T) {
		m, err := evaluateProbeStreams(format, "0.77", []probeStream{
			{CodecType: "video", CodecName: "hevc", Width: 1080, Height: 1920, NBFrames: "23"},
			{CodecType: "audio", CodecName: "aac"},
			{CodecType: "audio", CodecName: "apac"},
		})
		if err != nil || m.Duration != 0.77 {
			t.Fatalf("got %+v err=%v", m, err)
		}
	})
	t.Run("hevc_only_apac_still_ok", func(t *testing.T) {
		// Spatial-only audio is skipped; video-only upload is accepted.
		if _, err := evaluateProbeStreams(format, "1.0", []probeStream{
			{CodecType: "video", CodecName: "hevc", Width: 1080, Height: 1920},
			{CodecType: "audio", CodecName: "apac"},
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("two_primary_videos_rejected", func(t *testing.T) {
		_, err := evaluateProbeStreams(format, "1.0", []probeStream{
			{CodecType: "video", CodecName: "hevc", Width: 640, Height: 360},
			{CodecType: "video", CodecName: "h264", Width: 640, Height: 360},
		})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("oversize_rejected", func(t *testing.T) {
		_, err := evaluateProbeStreams(format, "1.0", []probeStream{
			{CodecType: "video", CodecName: "hevc", Width: 1080, Height: 1921},
		})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err=%v", err)
		}
	})
}
