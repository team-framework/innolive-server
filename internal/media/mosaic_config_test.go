package media

import (
	"context"
	"errors"
	"math"
	"testing"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func float64Ptr(value float64) *float64 { return &value }
func uint32Ptr(value uint32) *uint32    { return &value }

// TestMosaicConfigValidateMatchesAIRanges는 AI service/mosaic.py와 같은 경계에서
// 거절하는지 본다. 하나라도 어긋나면 서버가 받은 값이 AI에서 세션을 잠근다.
func TestMosaicConfigValidateMatchesAIRanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   MosaicConfig
		field string
	}{
		{name: "empty", cfg: MosaicConfig{}},
		{name: "upper bounds", cfg: MosaicConfig{BlurRadius: float64Ptr(64), PixelSize: uint32Ptr(8), MaskMargin: uint32Ptr(64)}},
		{name: "lower bounds", cfg: MosaicConfig{BlurRadius: float64Ptr(0.01), PixelSize: uint32Ptr(1), MaskMargin: uint32Ptr(0)}},
		{name: "zero radius", cfg: MosaicConfig{BlurRadius: float64Ptr(0)}, field: "blur_radius"},
		{name: "negative radius", cfg: MosaicConfig{BlurRadius: float64Ptr(-1)}, field: "blur_radius"},
		{name: "radius above 64", cfg: MosaicConfig{BlurRadius: float64Ptr(64.0001)}, field: "blur_radius"},
		{name: "NaN radius", cfg: MosaicConfig{BlurRadius: float64Ptr(math.NaN())}, field: "blur_radius"},
		{name: "infinite radius", cfg: MosaicConfig{BlurRadius: float64Ptr(math.Inf(1))}, field: "blur_radius"},
		{name: "zero pixel size", cfg: MosaicConfig{PixelSize: uint32Ptr(0)}, field: "pixel_size"},
		{name: "pixel size above 8", cfg: MosaicConfig{PixelSize: uint32Ptr(9)}, field: "pixel_size"},
		{name: "margin above 64", cfg: MosaicConfig{MaskMargin: uint32Ptr(65)}, field: "mask_margin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var invalid *MosaicConfigError
			if !errors.As(err, &invalid) || invalid.Field != tc.field {
				t.Fatalf("Validate() = %v, want %s error", err, tc.field)
			}
			if !errors.Is(err, ErrInvalidMosaicConfig) {
				t.Fatalf("errors.Is(%v, ErrInvalidMosaicConfig) = false", err)
			}
		})
	}
}

func TestMosaicConfigAIRequestKeepsUnsetFieldsUnset(t *testing.T) {
	if got := (MosaicConfig{}).aiRequest(1280, 720, 0, 0); got != nil {
		t.Fatalf("empty config request = %v, want nil so field 7 stays off the wire", got)
	}
	got := MosaicConfig{MaskMargin: uint32Ptr(0)}.aiRequest(1280, 720, 0, 0)
	if got == nil || got.MaskMargin == nil || got.GetMaskMargin() != 0 {
		t.Fatalf("zero margin request = %v, want explicit 0", got)
	}
	if got.BlurRadius != nil || got.PixelSize != nil {
		t.Fatalf("unset fields were sent: %v", got)
	}
}

func TestAIMaskMarginScalesToAIInputPixels(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		margin                        uint32
		inWidth, inHeight, outW, outH uint16
		want                          uint32
	}{
		{name: "no egress size", margin: 16, inWidth: 1920, inHeight: 1080, want: 16},
		{name: "same size", margin: 16, inWidth: 1280, inHeight: 720, outW: 1280, outH: 720, want: 16},
		{name: "AI larger than egress", margin: 16, inWidth: 1920, inHeight: 1080, outW: 1280, outH: 720, want: 24},
		{name: "AI smaller than egress", margin: 16, inWidth: 1280, inHeight: 720, outW: 1920, outH: 1080, want: 11},
		{name: "portrait letterboxed in landscape", margin: 10, inWidth: 720, inHeight: 1280, outW: 1280, outH: 720, want: 18},
		{name: "zero stays zero", margin: 0, inWidth: 1920, inHeight: 1080, outW: 1280, outH: 720, want: 0},
		{name: "clamped to AI limit", margin: 64, inWidth: 1920, inHeight: 1080, outW: 1280, outH: 720, want: MaxMosaicMaskMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aiMaskMargin(tc.margin, tc.inWidth, tc.inHeight, tc.outW, tc.outH); got != tc.want {
				t.Fatalf("aiMaskMargin() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRealProcessorSendsMosaicConfigEveryFrame(t *testing.T) {
	ai := &fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		return &aiv1.ProcessedVideoChunk{Data: []byte("out"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	process := func(timestamp int64) *aiv1.MosaicConfig {
		t.Helper()
		if _, err := processor.Process(context.Background(), []byte("in"), timestamp, 1920, 1080); err != nil {
			t.Fatal(err)
		}
		return ai.lastMosaic
	}

	if got := process(1); got != nil {
		t.Fatalf("default mosaic = %v, want nil", got)
	}
	before := processor.PrivacyGeneration()
	processor.SetEgressVideoSize("1280x720")
	processor.SetMosaicConfig(MosaicConfig{PixelSize: uint32Ptr(4), MaskMargin: uint32Ptr(16)})
	if processor.PrivacyGeneration() == before {
		t.Fatal("changing the mosaic config must start a new privacy generation")
	}
	for timestamp := int64(2); timestamp <= 3; timestamp++ {
		got := process(timestamp)
		if got.GetPixelSize() != 4 || got.GetMaskMargin() != 24 || got.BlurRadius != nil {
			t.Fatalf("frame %d mosaic = %v, want pixel_size 4, mask_margin 24 (1080p input), blur unset", timestamp, got)
		}
	}
	unchanged := processor.PrivacyGeneration()
	processor.SetMosaicConfig(MosaicConfig{PixelSize: uint32Ptr(4), MaskMargin: uint32Ptr(16)})
	if processor.PrivacyGeneration() != unchanged {
		t.Fatal("re-applying the same mosaic config must keep the privacy generation")
	}

	processor.SetMosaicConfig(MosaicConfig{})
	if got := process(4); got != nil {
		t.Fatalf("reset mosaic = %v, want nil", got)
	}
}

func TestProcessorMosaicConfigIsCopied(t *testing.T) {
	ai := &fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		return &aiv1.ProcessedVideoChunk{Data: []byte("out"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	margin := uint32(8)
	processor.SetMosaicConfig(MosaicConfig{MaskMargin: &margin})
	margin = 99
	if _, err := processor.Process(context.Background(), []byte("in"), 1, 1280, 720); err != nil {
		t.Fatal(err)
	}
	if got := ai.lastMosaic.GetMaskMargin(); got != 8 {
		t.Fatalf("mask_margin = %d, want 8 from the value at SetMosaicConfig time", got)
	}
}
