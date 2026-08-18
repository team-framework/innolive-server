package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type AIStream interface {
	Process(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error)
	Close()
}

var errAIInputPaused = errors.New("AI input is paused")

type Processor struct {
	mode                 config.PrivacyMode
	fixedDelay           time.Duration
	aiMu                 sync.RWMutex
	ai                   AIStream
	aiInputPaused        bool
	anonymizationEnabled bool
	metrics              *metrics.Registry
	logger               *slog.Logger
	wireFormat           config.WireFormat
	failurePolicy        config.AIFailurePolicy

	// timeoutLatchThreshold bounds how many CONSECUTIVE timeout failures are
	// tolerated (serving a per-frame blackout, retrying the AI next frame)
	// before the permanent latch fires. This keeps a transient AI-worker
	// thread-pool squeeze (a frame briefly queued past AI_GRPC_TIMEOUT) from
	// permanently blackening a session the way a genuine defect does. 0 means
	// even a single timeout latches immediately (legacy behavior).
	timeoutLatchThreshold int
	consecutiveTimeouts   atomic.Int64

	// fallback latches permanently once the AI boundary fails: the session
	// emits black frames from then on instead of raw or frozen video,
	// matching the Python server's fail-closed blackout semantics.
	fallback     atomic.Bool
	blackoutMu   sync.Mutex
	blackoutData []byte
}

func NewProcessor(
	mode config.PrivacyMode,
	fixedDelay time.Duration,
	ai AIStream,
	registry *metrics.Registry,
	logger *slog.Logger,
	wireFormat config.WireFormat,
	failurePolicy config.AIFailurePolicy,
	timeoutLatchThreshold int,
) (*Processor, error) {
	if mode == config.PrivacyModeReal && ai == nil {
		return nil, errors.New("real privacy mode requires an AI stream")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if wireFormat == "" {
		wireFormat = config.WireFormatJPEG
	}
	if failurePolicy == "" {
		failurePolicy = config.FailurePolicyBlackoutLatch
	}
	if timeoutLatchThreshold < 0 {
		timeoutLatchThreshold = 0
	}
	return &Processor{
		mode:                  mode,
		fixedDelay:            fixedDelay,
		ai:                    ai,
		metrics:               registry,
		logger:                logger,
		wireFormat:            wireFormat,
		failurePolicy:         failurePolicy,
		timeoutLatchThreshold: timeoutLatchThreshold,
		anonymizationEnabled:  mode == config.PrivacyModeReal,
	}, nil
}

func (p *Processor) Close() {
	p.aiMu.Lock()
	defer p.aiMu.Unlock()
	if p.ai != nil {
		p.ai.Close()
	}
}

// SuspendAIInput은 실제 AI 모드에서 새 카메라 프레임의 AI 전송을 중단하고
// 열려 있던 bidi stream을 닫는다. 이미 진행 중인 Process 호출이 끝날 때까지
// 기다리므로, 이 메서드가 반환된 뒤에는 새 프레임이 AI worker에 도달하지 않는다.
func (p *Processor) SuspendAIInput() {
	if p.mode != config.PrivacyModeReal {
		return
	}
	p.aiMu.Lock()
	defer p.aiMu.Unlock()
	p.aiInputPaused = true
	if p.ai != nil {
		p.ai.Close()
	}
}

// ResumeAIInput은 AI 입력 차단을 해제한다. AI Stream은 다음 Process 호출에서
// 필요한 경우 bidi stream을 다시 열므로, WebRTC·FFmpeg 파이프라인을 새로
// 만들지 않아도 된다.
func (p *Processor) ResumeAIInput() {
	if p.mode != config.PrivacyModeReal {
		return
	}
	p.aiMu.Lock()
	p.aiInputPaused = false
	p.aiMu.Unlock()
}

// SetAnonymizationEnabled changes whether frames are sent to the AI worker.
// Unlike SuspendAIInput (used by broadcast pause), disabling anonymization
// preserves the open bidi stream and passes the raw frame to the outputs.
func (p *Processor) SetAnonymizationEnabled(enabled bool) {
	if p.mode != config.PrivacyModeReal {
		return
	}
	p.aiMu.Lock()
	p.anonymizationEnabled = enabled
	p.aiMu.Unlock()
}

// AnonymizationEnabled reports whether real-mode frames are currently sent to
// the AI worker. Non-real privacy modes have no AI processing to toggle.
func (p *Processor) AnonymizationEnabled() bool {
	if p.mode != config.PrivacyModeReal {
		return false
	}
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	return p.anonymizationEnabled
}

// AIInputPaused는 실제 AI worker에 카메라 프레임을 보내지 않는 상태인지
// 반환한다. bypass·fixed-delay 모드는 AI 입력이 없으므로 항상 false다.
func (p *Processor) AIInputPaused() bool {
	if p.mode != config.PrivacyModeReal {
		return false
	}
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	return p.aiInputPaused
}

// FallbackActive reports whether the fail-closed blackout latch has fired.
func (p *Processor) FallbackActive() bool { return p.fallback.Load() }

func (p *Processor) Process(ctx context.Context, frame []byte, timestamp int64, width, height uint16) ([]byte, error) {
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	if p.mode == config.PrivacyModeReal && p.aiInputPaused {
		return nil, errAIInputPaused
	}
	if p.mode == config.PrivacyModeReal && !p.anonymizationEnabled {
		return frame, nil
	}
	return p.process(ctx, frame, timestamp, width, height)
}

// ProcessIfAIInputEnabled는 AI 입력이 중단된 pause 구간에는 processed=false를
// 반환한다. 읽기 잠금을 Process 전체에 유지해 SuspendAIInput 반환 뒤에도
// 직전에 통과한 프레임이 AI worker로 전송되는 경합을 막는다.
func (p *Processor) ProcessIfAIInputEnabled(ctx context.Context, frame []byte, timestamp int64, width, height uint16) ([]byte, bool, error) {
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	if p.mode == config.PrivacyModeReal && p.aiInputPaused {
		return nil, false, nil
	}
	if p.mode == config.PrivacyModeReal && !p.anonymizationEnabled {
		return frame, true, nil
	}
	output, err := p.process(ctx, frame, timestamp, width, height)
	return output, true, err
}

func (p *Processor) process(ctx context.Context, frame []byte, timestamp int64, width, height uint16) ([]byte, error) {
	startedAt := time.Now()

	switch p.mode {
	case config.PrivacyModeBypass:
		defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
		return frame, nil
	case config.PrivacyModeFixedDelay:
		defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
		timer := time.NewTimer(p.fixedDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return frame, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case config.PrivacyModeReal:
		if p.fallback.Load() {
			return p.serveBlackout(frame, width, height, nil)
		}
		output, err := p.processImage(frame, timestamp)
		if err == nil {
			p.consecutiveTimeouts.Store(0)
			return output, nil
		}
		if p.failurePolicy == config.FailurePolicyFreeze {
			return nil, err
		}
		// Tolerate a bounded run of consecutive timeouts (e.g. the AI worker's
		// thread pool momentarily saturated) without permanently latching:
		// serve a blackout frame for this frame only and retry the AI next
		// frame. A non-timeout failure resets straight to the latch path.
		if isTimeoutError(err) {
			if int(p.consecutiveTimeouts.Add(1)) <= p.timeoutLatchThreshold {
				return p.serveBlackout(frame, width, height, err)
			}
		} else {
			p.consecutiveTimeouts.Store(0)
		}
		if p.fallback.CompareAndSwap(false, true) {
			p.metrics.IncAIFallbackLatched(string(p.mode))
			p.logger.Warn("AI processing failed; latching fail-closed blackout for this session",
				"error", err,
				"wire_format", p.wireFormat,
				"hint", latchHint(err, p.wireFormat))
		}
		return p.serveBlackout(frame, width, height, err)
	default:
		return nil, fmt.Errorf("unsupported privacy mode %q", p.mode)
	}
}

func (p *Processor) ProcessImage(frame []byte, timestamp int64) ([]byte, error) {
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	if p.aiInputPaused {
		return nil, errAIInputPaused
	}
	return p.processImage(frame, timestamp)
}

func (p *Processor) processImage(frame []byte, timestamp int64) ([]byte, error) {
	startedAt := time.Now()
	defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
	if p.mode != config.PrivacyModeReal {
		return nil, fmt.Errorf("ProcessDecoded is only valid in real mode")
	}

	aiStartedAt := time.Now()
	response, err := p.ai.Process(frame, timestamp)
	p.metrics.ObserveAI(string(p.mode), time.Since(aiStartedAt))
	p.metrics.ObserveStage("grpc", time.Since(aiStartedAt))
	if err != nil {
		return nil, err
	}
	if response.GetTimestamp() != timestamp {
		return nil, fmt.Errorf("AI response timestamp mismatch: sent=%d received=%d", timestamp, response.GetTimestamp())
	}
	// error_code is set when the AI server completed the RPC but the frame
	// itself failed processing (e.g. decode failure) — the call succeeding
	// at the transport level does not mean the frame was safely processed,
	// so this must latch fail-closed exactly like a transport error does.
	if response.GetErrorCode() != "" {
		return nil, fmt.Errorf("AI processing failed: error_code=%s error_message=%q", response.GetErrorCode(), response.GetErrorMessage())
	}
	if !strings.EqualFold(response.GetStatusMessage(), "success") {
		return nil, fmt.Errorf("AI processing failed: status=%q", response.GetStatusMessage())
	}
	if len(response.GetData()) == 0 {
		return nil, errors.New("AI processing returned an empty frame")
	}
	return response.GetData(), nil
}

// serveBlackout returns the cached black frame for this session. If the
// black frame cannot be built, the original AI error (when present) is
// propagated so the frame is dropped rather than emitted raw.
func (p *Processor) serveBlackout(reference []byte, width, height uint16, cause error) ([]byte, error) {
	black, err := p.blackoutFrame(reference, width, height)
	if err != nil {
		if cause != nil {
			return nil, cause
		}
		return nil, err
	}
	p.metrics.IncAIFallbackFrame(string(p.mode))
	return black, nil
}

func (p *Processor) blackoutFrame(reference []byte, width, height uint16) ([]byte, error) {
	p.blackoutMu.Lock()
	defer p.blackoutMu.Unlock()
	if p.blackoutData != nil {
		return p.blackoutData, nil
	}

	if p.wireFormat == config.WireFormatRaw {
		size := rawFrameSize(width, height)
		if size <= 0 {
			return nil, errors.New("cannot determine blackout frame dimensions")
		}
		buffer := make([]byte, size)
		luma := int(width) * int(height)
		for i := 0; i < luma; i++ {
			buffer[i] = 0x10
		}
		for i := luma; i < size; i++ {
			buffer[i] = 0x80
		}
		p.blackoutData = buffer
		return buffer, nil
	}

	targetWidth, targetHeight := int(width), int(height)
	if decoded, err := jpeg.DecodeConfig(bytes.NewReader(reference)); err == nil {
		targetWidth, targetHeight = decoded.Width, decoded.Height
	}
	if targetWidth <= 0 || targetHeight <= 0 {
		return nil, errors.New("cannot determine blackout frame dimensions")
	}
	black := image.NewYCbCr(image.Rect(0, 0, targetWidth, targetHeight), image.YCbCrSubsampleRatio420)
	for i := range black.Y {
		black.Y[i] = 0x10
	}
	for i := range black.Cb {
		black.Cb[i] = 0x80
	}
	for i := range black.Cr {
		black.Cr[i] = 0x80
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, black, &jpeg.Options{Quality: 75}); err != nil {
		return nil, fmt.Errorf("encode blackout frame: %w", err)
	}
	p.blackoutData = encoded.Bytes()
	return p.blackoutData, nil
}

// isTimeoutError reports whether an AI-boundary error is a deadline/timeout,
// as opposed to a definitive rejection (status != success, empty frame) or a
// transport error. Timeouts are the one failure class that a transient AI
// worker overload can produce, so they are treated as potentially recoverable.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return status.Code(err) == codes.DeadlineExceeded
}

// latchHint returns an operator-facing hint tailored to the failure that is
// about to latch the session, so a wire-format explanation is not appended to
// failures (timeouts, empty frames) that have nothing to do with pixel format.
func latchHint(err error, wireFormat config.WireFormat) string {
	switch {
	case isTimeoutError(err):
		return "AI round-trip exceeded AI_GRPC_TIMEOUT past AI_TIMEOUT_LATCH_THRESHOLD; a single Python AI worker caps near 10 concurrent streams — scale the AI_GRPC_TARGETS pool or raise AI_GRPC_TIMEOUT"
	case wireFormat == config.WireFormatRaw:
		return "if the AI server cannot decode headerless raw yuv420p (PIL/OpenCV servers only accept self-describing images), set AI_FRAME_WIRE_FORMAT=jpeg"
	default:
		return "verify the AI server returns status_message=\"success\", echoes the request timestamp, and sends non-empty frame data"
	}
}
