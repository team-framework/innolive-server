package media

import (
	"errors"
	"log/slog"
	"math"

	aiv1 "inno-live-server/api/gen/aiv1"
)

// AI MosaicConfig 계약의 허용 범위다(innolive-ai service/mosaic.py). 범위 밖 값이
// 한 번이라도 AI에 가면 그 프레임이 MOSAIC_CONFIG_INVALID로 실패하고, Processor는
// error_code를 보고 세션을 fail-closed 블랙아웃으로 잠근다.
const (
	MaxMosaicBlurRadius = 64.0
	MinMosaicPixelSize  = 1
	MaxMosaicPixelSize  = 8
	MaxMosaicMaskMargin = 64
)

var ErrInvalidMosaicConfig = errors.New("invalid mosaic config")

// MosaicConfigError는 범위를 벗어난 필드 하나다. errors.Is(err,
// ErrInvalidMosaicConfig)가 참이다.
type MosaicConfigError struct {
	Field  string
	Reason string
}

func (e *MosaicConfigError) Error() string { return e.Field + " " + e.Reason }

func (e *MosaicConfigError) Is(target error) bool { return target == ErrInvalidMosaicConfig }

// MosaicConfig는 세션의 얼굴·번호판 블러 강도와 영역이다(#483). nil 필드는 AI
// 기본값(blur_radius 24, pixel_size 2, mask_margin 8)이며 0을 보내는 것과 다르다.
// AI는 스트림에 설정을 기억하지 않으므로 Processor가 매 프레임 싣는다.
type MosaicConfig struct {
	BlurRadius *float64 `json:"blur_radius"`
	PixelSize  *uint32  `json:"pixel_size"`
	// MaskMargin은 송출 영상 기준 픽셀이다. AI 입력 프레임과 크기가 다르면
	// 프레임마다 AI 입력 픽셀로 바꿔 보낸다.
	MaskMargin *uint32 `json:"mask_margin"`
}

// Validate는 AI가 거절할 값을 미리 거른다.
func (c MosaicConfig) Validate() error {
	if r := c.BlurRadius; r != nil && (math.IsNaN(*r) || math.IsInf(*r, 0) || *r <= 0 || *r > MaxMosaicBlurRadius) {
		return &MosaicConfigError{Field: "blur_radius", Reason: "must be a finite number in (0, 64]"}
	}
	if p := c.PixelSize; p != nil && (*p < MinMosaicPixelSize || *p > MaxMosaicPixelSize) {
		return &MosaicConfigError{Field: "pixel_size", Reason: "must be an integer in 1..8"}
	}
	if m := c.MaskMargin; m != nil && *m > MaxMosaicMaskMargin {
		return &MosaicConfigError{Field: "mask_margin", Reason: "must be an integer in 0..64"}
	}
	return nil
}

// Clone은 포인터를 공유하지 않는 사본이다.
func (c MosaicConfig) Clone() MosaicConfig {
	var clone MosaicConfig
	if c.BlurRadius != nil {
		value := *c.BlurRadius
		clone.BlurRadius = &value
	}
	if c.PixelSize != nil {
		value := *c.PixelSize
		clone.PixelSize = &value
	}
	if c.MaskMargin != nil {
		value := *c.MaskMargin
		clone.MaskMargin = &value
	}
	return clone
}

// Equal은 두 설정이 AI에 같은 값을 보내는지다. nil과 0은 다르다.
func (c MosaicConfig) Equal(other MosaicConfig) bool {
	return equalPointer(c.BlurRadius, other.BlurRadius) &&
		equalPointer(c.PixelSize, other.PixelSize) &&
		equalPointer(c.MaskMargin, other.MaskMargin)
}

func equalPointer[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// LogValue는 로그에 포인터 주소 대신 값을 남긴다. 비운 필드는 "default"다.
func (c MosaicConfig) LogValue() slog.Value {
	field := func(key string, value any, set bool) slog.Attr {
		if !set {
			return slog.String(key, "default")
		}
		return slog.Any(key, value)
	}
	var radius float64
	var size, margin uint32
	if c.BlurRadius != nil {
		radius = *c.BlurRadius
	}
	if c.PixelSize != nil {
		size = *c.PixelSize
	}
	if c.MaskMargin != nil {
		margin = *c.MaskMargin
	}
	return slog.GroupValue(
		field("blur_radius", radius, c.BlurRadius != nil),
		field("pixel_size", size, c.PixelSize != nil),
		field("mask_margin", margin, c.MaskMargin != nil),
	)
}

// aiRequest는 한 프레임에 실을 mosaic_config다. 아무것도 지정하지 않았으면 nil이라
// field 7이 빠지고 요청이 #483 이전과 바이트 단위로 같다. inWidth·inHeight는 AI
// 입력 프레임, outWidth·outHeight는 송출 영상 크기이며 0이면 둘이 같다고 본다.
func (c MosaicConfig) aiRequest(inWidth, inHeight, outWidth, outHeight uint16) *aiv1.MosaicConfig {
	if c.BlurRadius == nil && c.PixelSize == nil && c.MaskMargin == nil {
		return nil
	}
	request := &aiv1.MosaicConfig{}
	if c.BlurRadius != nil {
		value := *c.BlurRadius
		request.BlurRadius = &value
	}
	if c.PixelSize != nil {
		value := *c.PixelSize
		request.PixelSize = &value
	}
	if c.MaskMargin != nil {
		value := aiMaskMargin(*c.MaskMargin, inWidth, inHeight, outWidth, outHeight)
		request.MaskMargin = &value
	}
	return request
}

// aiMaskMargin은 송출 영상 기준 margin을 AI 입력 픽셀로 바꾼다. egress는
// EGRESS_VIDEO_SIZE에 종횡비를 지켜 맞추므로(force_original_aspect_ratio=decrease)
// 입력→송출 배율은 min(out/in)이고, 송출에서 m픽셀은 입력에서 m/배율이다. 크기를
// 모르면 그대로 둔다. 바꾼 값이 AI 상한을 넘으면 상한으로 자른다.
func aiMaskMargin(margin uint32, inWidth, inHeight, outWidth, outHeight uint16) uint32 {
	if inWidth == 0 || inHeight == 0 || outWidth == 0 || outHeight == 0 {
		return margin
	}
	scale := math.Max(float64(inWidth)/float64(outWidth), float64(inHeight)/float64(outHeight))
	converted := math.Round(float64(margin) * scale)
	if converted > MaxMosaicMaskMargin {
		return MaxMosaicMaskMargin
	}
	return uint32(converted)
}
