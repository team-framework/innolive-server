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
	Process(data []byte, timestamp int64, width, height uint16, pixFmt string) (*aiv1.ProcessedVideoChunk, error)
	Close()
}

var errAIInputPaused = errors.New("AI input is paused")

// defaultRecoveryProbeInterval은 잠긴 세션이 회복을 확인하려고 AI 경계를 다시
// 시도하는 주기다. 회복한 워커가 1초 안팎에 세션을 재개할 만큼 짧고, 잠긴 세션이
// 많아도 과부하 워커에 프레임마다 다시 몰리지 않을 만큼 길다.
const defaultRecoveryProbeInterval = time.Second

// privacyGenerations는 익명화 설정 세대를 매긴다. 프로세스 전역에서 단조
// 증가하므로 파이프라인이 새 Processor로 바뀌어도 이전 세대보다 크다(#311).
var privacyGenerations atomic.Uint64

type Processor struct {
	mode                 config.PrivacyMode
	fixedDelay           time.Duration
	aiMu                 sync.RWMutex
	ai                   AIStream
	aiInputPaused        bool
	anonymizationEnabled bool
	// privacyGeneration은 지금 익명화 설정의 세대다. 설정이 바뀔 때마다 새로
	// 받는다. egress는 이 세대로 정지 화면에 다시 써도 되는 프레임을 가린다.
	privacyGeneration atomic.Uint64
	metrics           *metrics.Registry
	logger            *slog.Logger
	wireFormat        config.WireFormat
	failurePolicy     config.AIFailurePolicy

	// timeoutLatchThreshold는 영구 잠금 전에 허용하는 연속 타임아웃 실패 횟수다(그
	// 동안은 프레임마다 블랙아웃을 내보내고 다음 프레임에 AI를 다시 시도한다). AI
	// 워커 스레드 풀이 잠깐 붐벼 프레임이 AI_GRPC_TIMEOUT을 넘긴 경우가 진짜 결함처럼
	// 세션을 영구히 검게 만들지 않게 한다. 0이면 타임아웃 한 번에 바로 잠근다(이전
	// 동작).
	timeoutLatchThreshold int
	consecutiveTimeouts   atomic.Int64

	// fallback은 AI 경계가 실패하면 세션을 fail-closed 블랙아웃으로 잠근다. 원본이나
	// 멈춘 영상 대신 검은 프레임이다. 잠금은 더는 영구가 아니다 — 잠긴 동안 AI를
	// recoveryProbeInterval마다 최대 한 번 다시 시도하고, 성공한 첫 프레임에서 잠금을
	// 풀고 정상 처리로 돌아간다. 워커 재시작 같은 일시적 AI 장애 뒤에 클라이언트
	// 재협상 없이 세션을 되살리면서, 시도 사이의 프레임은 계속 fail-closed다.
	fallback atomic.Bool
	// recoveryProbeInterval은 잠긴 세션이 AI를 다시 시도하는 빈도의 한계다. 잠긴
	// 세션이 많아도 이미 과부하인 워커에 프레임마다 몰리지 않게 한다. lastProbeAt은
	// 세션당 하나인 처리 고루틴에서만 건드린다.
	recoveryProbeInterval time.Duration
	lastProbeAt           time.Time
	blackoutMu            sync.Mutex
	blackoutData          []byte

	// mosaicTally는 세션의 블러 분류 누계다(#412). 없으면 Prometheus 카운터만 센다.
	mosaicTally atomic.Pointer[MosaicTally]
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
	processor := &Processor{
		mode:                  mode,
		fixedDelay:            fixedDelay,
		ai:                    ai,
		metrics:               registry,
		logger:                logger,
		wireFormat:            wireFormat,
		failurePolicy:         failurePolicy,
		timeoutLatchThreshold: timeoutLatchThreshold,
		recoveryProbeInterval: defaultRecoveryProbeInterval,
		anonymizationEnabled:  mode == config.PrivacyModeReal,
	}
	processor.privacyGeneration.Store(privacyGenerations.Add(1))
	return processor, nil
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

// SetAnonymizationEnabled는 프레임을 AI 워커로 보낼지 바꾼다. 방송 일시 정지가 쓰는
// SuspendAIInput과 달리, 익명화를 끄면 양방향 스트림은 열어 둔 채 원본 프레임을
// 출력으로 넘긴다.
func (p *Processor) SetAnonymizationEnabled(enabled bool) {
	if p.mode != config.PrivacyModeReal {
		return
	}
	p.aiMu.Lock()
	if p.anonymizationEnabled != enabled {
		p.privacyGeneration.Store(privacyGenerations.Add(1))
	}
	p.anonymizationEnabled = enabled
	p.aiMu.Unlock()
}

// PrivacyGeneration은 지금 익명화 설정의 세대다. 설정이 바뀌면 커진다.
// 쓰기는 aiMu 쓰기 잠금 안에서만 일어나고 처리 중에는 읽기 잠금을 쥐므로,
// 처리 전후에 읽은 값이 같으면 그 프레임은 한 설정 아래에서 처리된 것이다.
func (p *Processor) PrivacyGeneration() uint64 {
	return p.privacyGeneration.Load()
}

// AnonymizationEnabled는 real 모드 프레임을 지금 AI 워커로 보내는지다. real이 아닌
// 프라이버시 모드에는 켜고 끌 AI 처리가 없다.
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

// FallbackActive는 fail-closed 블랙아웃 잠금이 걸렸는지다.
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
			// 잠김: AI를 recoveryProbeInterval마다 최대 한 번만 다시 시도해, 잠긴 세션이
			// 모두 과부하 워커에 몰리지 않으면서 회복한 워커를 알아챈다. 시도 사이에는
			// AI를 건드리지 않고 블랙아웃을 내보낸다.
			if time.Since(p.lastProbeAt) < p.recoveryProbeInterval {
				return p.serveBlackout(frame, width, height, nil)
			}
			p.lastProbeAt = time.Now()
			output, err := p.processImage(frame, timestamp, width, height)
			if err != nil {
				return p.serveBlackout(frame, width, height, err)
			}
			// AI 경계가 다시 동작한다. 잠금을 풀고 이 프레임부터 정상 처리한다.
			p.fallback.Store(false)
			p.consecutiveTimeouts.Store(0)
			p.metrics.IncAIFallbackRecovered(string(p.mode))
			p.logger.Info("AI processing recovered; clearing fail-closed blackout latch for this session")
			return output, nil
		}
		output, err := p.processImage(frame, timestamp, width, height)
		if err == nil {
			p.consecutiveTimeouts.Store(0)
			return output, nil
		}
		// 파이프라인을 내리며(해상도 전환·세션 종료) 서버가 스스로 끊은 요청이다.
		// AI 실패가 아니므로 latch·카운터 없이 돌려준다(#297).
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if p.failurePolicy == config.FailurePolicyFreeze {
			return nil, err
		}
		// 연속 타임아웃이 일정 횟수까지는(예: AI 워커 스레드 풀이 잠깐 포화) 영구히
		// 잠그지 않는다. 이 프레임만 블랙아웃을 내보내고 다음 프레임에 AI를 다시
		// 시도한다. 타임아웃이 아닌 실패는 곧바로 잠금 경로로 간다.
		if isTimeoutError(err) {
			if int(p.consecutiveTimeouts.Add(1)) <= p.timeoutLatchThreshold {
				return p.serveBlackout(frame, width, height, err)
			}
		} else {
			p.consecutiveTimeouts.Store(0)
		}
		if p.fallback.CompareAndSwap(false, true) {
			p.lastProbeAt = time.Now()
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

func (p *Processor) ProcessImage(frame []byte, timestamp int64, width, height uint16) ([]byte, error) {
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	if p.aiInputPaused {
		return nil, errAIInputPaused
	}
	return p.processImage(frame, timestamp, width, height)
}

// aiPixFmt는 raw wire 포맷일 때 AI가 data를 해석할 픽셀 포맷을 돌려준다.
// JPEG는 자기서술적이라 빈 문자열이다.
func (p *Processor) aiPixFmt() string {
	if p.wireFormat == config.WireFormatRaw {
		return "yuv420p"
	}
	return ""
}

func (p *Processor) processImage(frame []byte, timestamp int64, width, height uint16) ([]byte, error) {
	startedAt := time.Now()
	defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
	if p.mode != config.PrivacyModeReal {
		return nil, fmt.Errorf("ProcessDecoded is only valid in real mode")
	}

	aiStartedAt := time.Now()
	response, err := p.ai.Process(frame, timestamp, width, height, p.aiPixFmt())
	p.metrics.ObserveAI(string(p.mode), time.Since(aiStartedAt))
	p.metrics.ObserveStage("grpc", time.Since(aiStartedAt))
	if err != nil {
		return nil, err
	}
	if response.GetTimestamp() != timestamp {
		return nil, fmt.Errorf("AI response timestamp mismatch: sent=%d received=%d", timestamp, response.GetTimestamp())
	}
	// error_code는 AI 서버가 RPC는 끝냈지만 프레임 처리 자체가 실패했을 때(예: 디코드
	// 실패) 설정된다. 전송 수준에서 호출이 성공했다고 프레임이 안전하게 처리된 것은
	// 아니므로, 전송 오류와 똑같이 fail-closed로 잠가야 한다.
	if response.GetErrorCode() != "" {
		return nil, fmt.Errorf("AI processing failed: error_code=%s error_message=%q", response.GetErrorCode(), response.GetErrorMessage())
	}
	if !strings.EqualFold(response.GetStatusMessage(), "success") {
		return nil, fmt.Errorf("AI processing failed: status=%q", response.GetStatusMessage())
	}
	if len(response.GetData()) == 0 {
		return nil, errors.New("AI processing returned an empty frame")
	}
	p.recordMosaic(response.GetFaces())
	return response.GetData(), nil
}

// SetMosaicTally는 이 Processor가 블러 분류를 더할 세션 누계를 정한다.
func (p *Processor) SetMosaicTally(tally *MosaicTally) {
	p.mosaicTally.Store(tally)
}

// recordMosaic은 AI 처리에 성공한 프레임의 객체 목록을 블러 기준으로 분류해
// 서버 합계와 세션 누계에 더한다.
func (p *Processor) recordMosaic(objects []*aiv1.FaceMetadata) {
	counts := classifyMosaic(objects)
	p.metrics.ObserveMosaic(counts.kind(), counts.faces, counts.plates, counts.whitelistedFaces)
	if tally := p.mosaicTally.Load(); tally != nil {
		tally.record(counts)
	}
}

// serveBlackout은 이 세션에 캐시한 검은 프레임을 돌려준다. 검은 프레임을 만들 수
// 없으면 원래 AI 오류(있으면)를 넘겨 프레임을 원본으로 내보내지 않고 버린다.
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

// isTimeoutError는 AI 경계 오류가 기한·타임아웃인지다. 확정적 거절(status가 success
// 아님, 빈 프레임)이나 전송 오류와 구분한다. 일시적 AI 워커 과부하가 만들 수 있는
// 실패는 타임아웃뿐이라 회복 가능성이 있는 것으로 다룬다.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return status.Code(err) == codes.DeadlineExceeded
}

// latchHint는 세션을 잠그려는 실패에 맞춘 운영자용 힌트를 돌려준다. 픽셀 형식과
// 무관한 실패(타임아웃·빈 프레임)에 와이어 형식 설명이 붙지 않게 한다.
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
