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
	// mosaic은 이 프레임에만 적용할 블러 설정이다. nil이면 AI 기본값이다.
	Process(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) (*aiv1.ProcessedVideoChunk, error)
	Close()
}

// AIPipeline은 앞 응답을 기다리지 않고 다음 프레임을 보낼 수 있는 AI 스트림이다(#495).
// SubmitFrame은 요청을 보내고 응답을 기다리는 함수를 돌려준다. 응답은 보낸 순서대로 온다.
type AIPipeline interface {
	SubmitFrame(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) (func() (*aiv1.ProcessedVideoChunk, error), error)
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
	// screenBlurEnabled는 스트리머가 고른 화면 전체 블러다. 켜져 있으면 AI 대신
	// 프레임 전체를 블러해 내보낸다(#465).
	screenBlurEnabled bool
	// mosaicConfig는 스트리머가 고른 블러 강도·영역이다(#483). AI가 기억하지
	// 않으므로 매 프레임 싣는다. egressWidth·Height는 mask_margin의 기준인 송출
	// 영상 크기(EGRESS_VIDEO_SIZE)이며, 0이면 AI 입력 프레임과 같다.
	mosaicConfig MosaicConfig
	egressWidth  uint16
	egressHeight uint16
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
	// 세션이 많아도 이미 과부하인 워커에 프레임마다 몰리지 않게 한다. AI 동시 요청이
	// 1보다 크면 요청을 보내는 고루틴과 결과를 마무리하는 고루틴이 함께 건드리므로
	// probeMu로 보호한다.
	recoveryProbeInterval time.Duration
	probeMu               sync.Mutex
	lastProbeAt           time.Time
	// aiMaxInflight는 세션당 AI에 동시에 보내 둘 프레임 수다(#495). 1이면 프레임마다
	// 응답을 기다리는 종전 처리다. AI 스트림이 AIPipeline일 때만 1보다 크다.
	aiMaxInflight int
	blackoutMu    sync.Mutex
	blackoutData  []byte
	// blackoutWidth·Height는 blackoutData를 만든 프레임 치수다. 입력 방향이 바뀌면
	// 디코더 출력 치수가 달라지므로(#449) 그때 다시 만든다.
	blackoutWidth  uint16
	blackoutHeight uint16

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
		aiMaxInflight:         1,
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

// SetScreenBlurEnabled는 화면 전체 블러를 켜고 끈다. 익명화 토글처럼 AI 스트림은
// 열어 둔다.
func (p *Processor) SetScreenBlurEnabled(enabled bool) {
	if p.mode != config.PrivacyModeReal {
		return
	}
	p.aiMu.Lock()
	if p.screenBlurEnabled != enabled {
		p.privacyGeneration.Store(privacyGenerations.Add(1))
	}
	p.screenBlurEnabled = enabled
	p.aiMu.Unlock()
}

// SetMosaicConfig는 다음 프레임부터 AI에 보낼 블러 설정을 바꾼다. 호출자가 범위를
// 검증해야 한다 — 범위 밖 값은 AI 오류로 세션을 블랙아웃으로 잠근다.
func (p *Processor) SetMosaicConfig(cfg MosaicConfig) {
	if p.mode != config.PrivacyModeReal {
		return
	}
	p.aiMu.Lock()
	if !p.mosaicConfig.Equal(cfg) {
		p.privacyGeneration.Store(privacyGenerations.Add(1))
	}
	p.mosaicConfig = cfg.Clone()
	p.aiMu.Unlock()
}

// SetEgressVideoSize는 mask_margin의 기준인 송출 영상 크기("1280x720")를 정한다.
// 비었거나 형식이 틀리면 AI 입력 프레임 크기를 그대로 기준으로 쓴다.
func (p *Processor) SetEgressVideoSize(value string) {
	width, height, _ := parseVideoSize(value)
	p.aiMu.Lock()
	p.egressWidth, p.egressHeight = width, height
	p.aiMu.Unlock()
}

// ScreenBlurEnabled는 real 모드 프레임을 지금 화면 전체 블러로 내보내는지다.
func (p *Processor) ScreenBlurEnabled() bool {
	if p.mode != config.PrivacyModeReal {
		return false
	}
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	return p.screenBlurEnabled
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
	if p.mode == config.PrivacyModeReal && p.screenBlurEnabled {
		return p.screenBlur(frame, width, height)
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
	return p.processIfAIInputEnabledLocked(ctx, frame, timestamp, width, height)
}

// SetAIMaxInflight는 세션당 AI에 동시에 보내 둘 프레임 수를 정한다(#495). AI 스트림이
// 미리 보내기(AIPipeline)를 지원하지 않거나 n이 1보다 작으면 1이다. 처리를 시작하기
// 전에 부른다.
func (p *Processor) SetAIMaxInflight(n int) {
	p.aiMu.Lock()
	defer p.aiMu.Unlock()
	if _, ok := p.ai.(AIPipeline); !ok || n < 1 {
		n = 1
	}
	p.aiMaxInflight = n
}

// AIMaxInflight는 세션당 AI에 동시에 보내 둘 프레임 수다.
func (p *Processor) AIMaxInflight() int {
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	return p.aiMaxInflight
}

// BeginIfAIInputEnabled는 ProcessIfAIInputEnabled를 시작과 마무리로 나눈다(#495). AI
// 동시 요청이 1보다 크고 프레임이 AI로 가야 하면 요청만 보내고, 돌려준 finish가 응답을
// 기다려 마무리한다. 그 밖의 경로는 여기서 끝까지 처리하고 finish는 결과만 돌려준다.
// 요청은 Begin을 부른 순서대로 AI에 가므로 finish도 시작한 순서대로 한 고루틴에서
// 부른다. 읽기 잠금은 요청을 보낼 때까지만 쥐어 SuspendAIInput 뒤에 프레임이 AI로
// 가지 않게 한다.
func (p *Processor) BeginIfAIInputEnabled(ctx context.Context, frame []byte, timestamp int64, width, height uint16) func() ([]byte, bool, error) {
	p.aiMu.RLock()
	pipeline, ok := p.ai.(AIPipeline)
	if !ok || p.aiMaxInflight <= 1 || p.mode != config.PrivacyModeReal || p.aiInputPaused ||
		p.screenBlurEnabled || !p.anonymizationEnabled || p.fallback.Load() {
		output, processed, err := p.processIfAIInputEnabledLocked(ctx, frame, timestamp, width, height)
		p.aiMu.RUnlock()
		return func() ([]byte, bool, error) { return output, processed, err }
	}
	startedAt := time.Now()
	mosaic := p.mosaicConfig.aiRequest(width, height, p.egressWidth, p.egressHeight)
	wait, err := pipeline.SubmitFrame(frame, timestamp, width, height, p.aiPixFmt(), mosaic)
	p.aiMu.RUnlock()
	return func() ([]byte, bool, error) {
		var response *aiv1.ProcessedVideoChunk
		if err == nil {
			response, err = wait()
		}
		output, err := p.acceptResponse(response, err, timestamp, startedAt)
		p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt))
		output, err = p.settle(frame, width, height, output, err)
		return output, true, err
	}
}

func (p *Processor) processIfAIInputEnabledLocked(ctx context.Context, frame []byte, timestamp int64, width, height uint16) ([]byte, bool, error) {
	if p.mode == config.PrivacyModeReal && p.aiInputPaused {
		return nil, false, nil
	}
	if p.mode == config.PrivacyModeReal && p.screenBlurEnabled {
		output, err := p.screenBlur(frame, width, height)
		return output, true, err
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
			if !p.probeDue() {
				return p.serveBlackout(frame, width, height, nil)
			}
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
		return p.settle(frame, width, height, output, err)
	default:
		return nil, fmt.Errorf("unsupported privacy mode %q", p.mode)
	}
}

// settle은 잠기지 않은 세션에서 AI를 거친 프레임 결과를 정한다. 실패면 정책에 따라
// 블랙아웃을 내거나 세션을 잠근다.
func (p *Processor) settle(frame []byte, width, height uint16, output []byte, err error) ([]byte, error) {
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
		p.probeMu.Lock()
		p.lastProbeAt = time.Now()
		p.probeMu.Unlock()
		p.metrics.IncAIFallbackLatched(string(p.mode))
		p.logger.Warn("AI processing failed; latching fail-closed blackout for this session",
			"error", err,
			"wire_format", p.wireFormat,
			"hint", latchHint(err, p.wireFormat))
	}
	return p.serveBlackout(frame, width, height, err)
}

// probeDue는 잠긴 세션이 지금 AI를 다시 시도할 차례인지 정하고, 차례면 시도 시각을
// 기록한다.
func (p *Processor) probeDue() bool {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if time.Since(p.lastProbeAt) < p.recoveryProbeInterval {
		return false
	}
	p.lastProbeAt = time.Now()
	return true
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
	mosaic := p.mosaicConfig.aiRequest(width, height, p.egressWidth, p.egressHeight)
	var response *aiv1.ProcessedVideoChunk
	var err error
	if pipeline, ok := p.ai.(AIPipeline); ok && p.aiMaxInflight > 1 {
		// 미리 보내기 스트림은 Process와 섞어 쓰지 않으므로 한 프레임도 같은 길로 보낸다.
		var wait func() (*aiv1.ProcessedVideoChunk, error)
		if wait, err = pipeline.SubmitFrame(frame, timestamp, width, height, p.aiPixFmt(), mosaic); err == nil {
			response, err = wait()
		}
	} else {
		response, err = p.ai.Process(frame, timestamp, width, height, p.aiPixFmt(), mosaic)
	}
	return p.acceptResponse(response, err, timestamp, aiStartedAt)
}

// acceptResponse는 AI 응답이 이 프레임의 정상 처리 결과인지 확인하고 블러 분류를 센다.
func (p *Processor) acceptResponse(response *aiv1.ProcessedVideoChunk, err error, timestamp int64, aiStartedAt time.Time) ([]byte, error) {
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
	p.metrics.ObserveFaceAnonymization(counts.swappedFaces, counts.blurFallbackFaces)
	if tally := p.mosaicTally.Load(); tally != nil {
		tally.record(counts)
	}
}

// serveBlackout은 AI를 거치지 않은 프레임 대신 내보낼 fail-closed 프레임을
// 돌려준다. raw wire면 화면 전체를 흐린 프레임이고(#463), 블러를 만들 수 없으면
// (JPEG wire·크기 불일치) 이 세션에 캐시한 검은 프레임이다. 둘 다 만들 수 없으면
// 원래 AI 오류(있으면)를 넘겨 프레임을 원본으로 내보내지 않고 버린다.
func (p *Processor) serveBlackout(reference []byte, width, height uint16, cause error) ([]byte, error) {
	if p.wireFormat == config.WireFormatRaw {
		if blurred, ok := blurRawFrame(reference, width, height); ok {
			p.metrics.IncAIFallbackFrame(string(p.mode))
			return blurred, nil
		}
	}
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

// screenBlur는 화면 전체 블러 프레임을 돌려준다. 블러는 raw 프레임에서만 할 수
// 있으므로 jpeg 와이어나 크기가 맞지 않는 프레임에는 원본 대신 검은 프레임을 낸다.
func (p *Processor) screenBlur(frame []byte, width, height uint16) ([]byte, error) {
	if p.wireFormat == config.WireFormatRaw {
		if blurred, ok := blurRawFrame(frame, width, height); ok {
			return blurred, nil
		}
	}
	return p.blackoutFrame(frame, width, height)
}

func (p *Processor) blackoutFrame(reference []byte, width, height uint16) ([]byte, error) {
	p.blackoutMu.Lock()
	defer p.blackoutMu.Unlock()
	if p.blackoutData != nil && p.blackoutWidth == width && p.blackoutHeight == height {
		return p.blackoutData, nil
	}
	p.blackoutWidth, p.blackoutHeight = width, height

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
