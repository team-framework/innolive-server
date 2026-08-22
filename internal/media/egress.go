package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

const (
	// egressQueueSize는 blur → egress 채널 크기를 제한한다. 가득 차면 가장 오래된
	// 프레임을 버려 blur 단계가 느린 RTMP 피어 때문에 막히지 않게 한다.
	egressQueueSize = 5
	// egressMeasureFrames는 첫 spawn 전에 RTP timestamp로 소스 프레임률을
	// 계산하기 위해 버퍼링할 선행 프레임 수다.
	egressMeasureFrames = 30
	egressDefaultFPS    = 30
	// egressVideoBitrate는 720p 이하 소스에 사용하며, 그보다 큰 FHD 소스에는
	// egressVideoBitrateFHD를 사용한다. EGRESS_VIDEO_BITRATE가 둘 다 덮어쓴다.
	egressVideoBitrate    = "2500k"
	egressVideoBitrateFHD = "5000k"
	egressAudioBitrate    = "128k"
	egressBackoffMin      = time.Second
	egressBackoffMax      = 30 * time.Second
	// egressReconnectMaxAttempts는 최초 실패 뒤 실제로 다시 FFmpeg를 시작하는
	// 횟수의 상한이다. 최초 시작 자체는 재시도가 아니므로 이 수에 포함하지 않는다.
	egressReconnectMaxAttempts = 6
	// egressReconnectMaxElapsed는 최초 송출 실패부터 egress가 재연결 상태에
	// 머물 수 있는 최대 시간이다. backoff뿐 아니라 새 시작을 기다리는 시간도
	// 이 예산 안에 들어간다.
	egressReconnectMaxElapsed = 90 * time.Second
	// egressStableFrames는 이후 실패 시 재연결 backoff를 최솟값으로 되돌리려면
	// 프로세스가 받아야 하는 프레임 수다.
	egressStableFrames = 300
)

var errReconnectInputTimeout = errors.New("timed out waiting for input frames while reconfiguring RTMP egress")

// EgressPhase는 RTMP egress의 수명 단계다.
type EgressPhase string

const (
	// EgressPhaseIdle: 첫 스폰 전, 프레임 수집·대기 중.
	EgressPhaseIdle EgressPhase = "idle"
	// EgressPhaseStreaming: FFmpeg 가동, 프레임 송출 중.
	EgressPhaseStreaming EgressPhase = "streaming"
	// EgressPhasePaused: 일시 중단 요청이 수락돼 미디어 경로가 취소 슬레이트와
	// 무음 오디오를 송출하는 상태.
	EgressPhasePaused EgressPhase = "paused"
	// EgressPhaseReconfiguring: 입력 프로필 변경을 감지해 새 해상도·FPS를
	// 측정하고 FFmpeg를 다시 시작하는 상태.
	EgressPhaseReconfiguring EgressPhase = "reconfiguring"
	// EgressPhasePausedReconfiguring: 취소 슬레이트를 유지한 채 새 입력
	// 프로필을 측정하는 상태.
	EgressPhasePausedReconfiguring EgressPhase = "paused_reconfiguring"
	// EgressPhaseReconnecting: 송출 실패 후 백오프 대기 중.
	EgressPhaseReconnecting EgressPhase = "reconnecting"
	// EgressPhasePausedReconnecting: 취소 슬레이트 상태를 보존한 채 RTMP
	// 재연결을 시도하는 상태.
	EgressPhasePausedReconnecting EgressPhase = "paused_reconnecting"
	// EgressPhaseStopped: Run 종료(세션 teardown 또는 명시적 중지).
	EgressPhaseStopped EgressPhase = "stopped"
)

// EgressStopReason은 egress 자체가 최종 종료된 이유다. 세션 삭제·로그아웃처럼
// 세션 계층이 결정하는 종료 사유와 달리, 이 값은 Run 고루틴이 스스로 포기했을
// 때도 StreamState에 남길 수 있다.
type EgressStopReason string

const (
	// EgressStopReasonReconnectExhausted는 RTMP 또는 FFmpeg 오류 재시도 예산을
	// 모두 소진했음을 뜻한다.
	EgressStopReasonReconnectExhausted EgressStopReason = "rtmp_reconnect_exhausted"
	// EgressStopReasonReconnectInputTimeout은 재구성·복구 경로에서 새 출력
	// 프로필을 만들 입력 프레임을 제한 시간 안에 얻지 못했음을 뜻한다.
	EgressStopReasonReconnectInputTimeout EgressStopReason = "reconnect_input_timeout"
)

// EgressStatus는 Status()가 반환하는 egress 상태 스냅샷이다. 세션 계층이
// StreamState 응답을 합성할 때 조회하는 pull 모델의 소스이며, egress 내부
// 고루틴이 콜백으로 세션 락을 잡는 역방향 결합을 만들지 않기 위한 구조다.
type EgressStatus struct {
	Phase             EgressPhase
	TargetURL         string // 마스킹한 출력 URL
	StartedAt         *time.Time
	StoppedAt         *time.Time
	UpdatedAt         time.Time
	LastError         *string
	StopReason        *EgressStopReason
	ReconnectAttempts int
	// Width, Height, FPS는 현재 측정된 egress 세대의 출력 형식이다. 일시
	// 중단 상태에서도 유지하여 RTMP 연결을 재시작하지 않고 정확히 같은
	// 형식의 슬레이트를 만들 수 있게 한다.
	Width  uint16
	Height uint16
	FPS    int
	// PausedAt은 마지막 일시 중단이 시작된 시각이다. resume과 stop 뒤에도
	// 마지막 일시 중단 이력으로 보존한다.
	PausedAt *time.Time
}

// egressTransitionEvent는 egress 상태를 바꾸는 외부 사건이다. 상태 전이는
// transition 한 곳에서만 허용한다. 따라서 pause/resume API, RTMP 오류,
// 입력 프로필 변경, 세션 종료가 서로 다른 규칙으로 Phase를 덮어쓰지 않는다.
type egressTransitionEvent uint8

const (
	egressTransitionProfileReady egressTransitionEvent = iota
	egressTransitionPause
	egressTransitionResume
	egressTransitionReconnect
	egressTransitionReconfigure
	egressTransitionStop
)

type egressTransition struct {
	event             egressTransitionEvent
	width             uint16
	height            uint16
	fps               int
	cause             error
	reason            EgressStopReason
	reconnectAttempts int
}

// reconnectPolicy는 한 egress 세대가 오류 뒤 자동으로 복구를 시도할 수 있는
// 예산이다. 운영 중에 임의로 바뀌지 않게 코드 상수로 두며, 조정이 필요해지면
// 설정 계약으로 승격한다.
type reconnectPolicy struct {
	maxAttempts int
	maxElapsed  time.Duration
	minBackoff  time.Duration
	maxBackoff  time.Duration
}

var defaultReconnectPolicy = reconnectPolicy{
	maxAttempts: egressReconnectMaxAttempts,
	maxElapsed:  egressReconnectMaxElapsed,
	minBackoff:  egressBackoffMin,
	maxBackoff:  egressBackoffMax,
}

// reconnectBudget은 현재 장애 구간의 재시도 횟수와 시작 시각을 보관한다.
// attempts는 오류 수가 아니라 실제로 다시 FFmpeg를 시작한 횟수다.
type reconnectBudget struct {
	policy      reconnectPolicy
	firstFailed time.Time
	attempts    int
}

func newReconnectBudget(policy reconnectPolicy) reconnectBudget {
	return reconnectBudget{policy: policy}
}

func (b *reconnectBudget) noteFailure(now time.Time) {
	if b.firstFailed.IsZero() {
		b.firstFailed = now
	}
}

func (b reconnectBudget) deadline() time.Time {
	if b.firstFailed.IsZero() {
		return time.Time{}
	}
	return b.firstFailed.Add(b.policy.maxElapsed)
}

// nextDelay는 다음 재시도 전의 대기 시간을 돌려준다. deadline을 넘기는
// backoff는 남은 시간까지만 기다리게 하며, 호출자는 그 뒤 BeginAttempt가
// false인지 확인해 새 프로세스를 시작하지 않는다.
func (b reconnectBudget) nextDelay(now time.Time) (time.Duration, bool) {
	if b.firstFailed.IsZero() || b.attempts >= b.policy.maxAttempts || !now.Before(b.deadline()) {
		return 0, false
	}
	delay := b.policy.minBackoff
	for attempt := 0; attempt < b.attempts; attempt++ {
		delay *= 2
		if delay >= b.policy.maxBackoff {
			delay = b.policy.maxBackoff
			break
		}
	}
	if remaining := b.deadline().Sub(now); remaining < delay {
		return remaining, true
	}
	return delay, true
}

// beginAttempt은 재연결용 FFmpeg 시작 직전에 호출한다. 시간·횟수 예산을 모두
// 확인한 뒤에만 attempts를 올려, StreamState가 실제 시작한 재시도 수를 보인다.
func (b *reconnectBudget) beginAttempt(now time.Time) bool {
	if b.firstFailed.IsZero() || b.attempts >= b.policy.maxAttempts || !now.Before(b.deadline()) {
		return false
	}
	b.attempts++
	return true
}

func (b *reconnectBudget) reset() {
	b.firstFailed = time.Time{}
	b.attempts = 0
}

// cachedCancellationSlate는 한 egress가 현재 출력 규격에서 재사용하는 취소
// 슬레이트다. 해상도 또는 wire format이 바뀌면 새 값으로 교체하며, 전역 map을
// 사용하지 않아 입력 규격 수에 비례해 메모리가 누적되지 않는다.
type cachedCancellationSlate struct {
	width  uint16
	height uint16
	format config.WireFormat
	frame  frame
}

// RTMPEgress는 FFmpegTranscoder 프로세스 패턴을 따라 전용 FFmpeg 자식 프로세스를
// 통해 처리된(blurred) 프레임을 RTMP endpoint로 보낸다.
//
// 오디오가 nil이거나 마이크 오디오를 아직 받지 못했으면 무음 오디오 트랙을
// 생성한다(YouTube는 오디오 트랙 없는 스트림을 거부한다). 송출자의 Opus 마이크가
// 흐르면 대신 fd 3(pipe:3)의 Ogg/Opus pipe를 통해 오디오를 전달한다.
type RTMPEgress struct {
	transcoder *FFmpegTranscoder
	logger     *slog.Logger
	metrics    *metrics.Registry
	wireFormat config.WireFormat
	outputURL  string
	audio      *AudioPipe
	// audioWriteEnd는 현재 FFmpeg 자식 프로세스에 연결된 pipe write end다.
	// 종료 시 후속 프로세스가 아니라 정확히 이 스트림만 분리한다.
	audioWriteEnd *os.File
	latency       *latencyTracker
	// audioOffset은 FFmpeg -itsoffset으로 마이크를 blur 지연된 영상에 맞춰
	// 지연시킨다. 양수 값은 오디오를 늦추며 EGRESS_AUDIO_OFFSET_MS로 런타임에
	// 조정할 수 있다.
	audioOffset time.Duration
	// bitrateOverride가 비어 있지 않으면(EGRESS_VIDEO_BITRATE) 해상도 기반
	// 영상 bitrate를 대체한다.
	bitrateOverride string
	// videoSize가 비어 있지 않으면(EGRESS_VIDEO_SIZE) 입력 프레임 해상도와
	// 무관하게 출력 해상도를 이 값으로 고정한다. WebRTC 퍼블리셔가 대역폭에
	// 따라 해상도를 바꿔도 RTMP 출력 프로필이 흔들리지 않게 한다.
	videoSize string
	input     chan frame
	// reconnectPolicy는 이 egress가 사용하는 자동 복구 예산이다. 기본값은
	// 코드 상수지만, 테스트에서는 짧은 정책으로 교체해 실제 Run 루프를 검증한다.
	reconnectPolicy reconnectPolicy
	// startProcess는 FFmpeg egress 프로세스를 만든다. 운영에서는 start를 쓰고,
	// 테스트에서는 시작·쓰기 실패를 외부 FFmpeg 없이 재현하는 double로 바꾼다.
	startProcess func(context.Context, uint16, uint16, int) (*ffmpegProcess, error)

	statusMu sync.Mutex
	status   EgressStatus
	slateMu  sync.Mutex
	slate    *cachedCancellationSlate
}

func NewRTMPEgress(path string, logger *slog.Logger, registry *metrics.Registry, options TranscoderOptions, outputURL string, audio *AudioPipe, latencyLog bool, audioOffset time.Duration, videoBitrate, videoSize string) *RTMPEgress {
	if options.WireFormat == "" {
		options.WireFormat = config.WireFormatJPEG
	}
	egressLogger := logger.With("ffmpeg_role", "egress")
	egress := &RTMPEgress{
		transcoder:      NewFFmpegTranscoder(path, logger, registry, options),
		logger:          egressLogger,
		metrics:         registry,
		wireFormat:      options.WireFormat,
		outputURL:       outputURL,
		audio:           audio,
		latency:         newLatencyTracker(egressLogger, latencyLog),
		audioOffset:     audioOffset,
		bitrateOverride: videoBitrate,
		videoSize:       videoSize,
		input:           make(chan frame, egressQueueSize),
		reconnectPolicy: defaultReconnectPolicy,
		status: EgressStatus{
			Phase:     EgressPhaseIdle,
			TargetURL: maskStreamKey(outputURL),
			UpdatedAt: time.Now().UTC(),
		},
	}
	egress.startProcess = egress.start
	return egress
}

// Status는 egress 상태 스냅샷을 반환한다. 어느 고루틴에서든 호출 가능하다.
func (e *RTMPEgress) Status() EgressStatus {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	return e.status
}

// transition은 EgressStatus.Phase를 바꾸는 유일한 경로다. 생성자에서의
// idle 초기화만 예외이며, 그 뒤에는 모든 상태 변경이 이 전이표를 거친다.
//
// profile_ready는 FFmpeg가 새 입력 프로필로 시작된 경우, reconnect는 RTMP 또는
// FFmpeg 오류로 재시도를 시작하는 경우를 뜻한다. stop은 모든 생존 상태에서
// 허용되고 멱등적으로 처리한다. 나머지 허용되지 않은 전이는 false를 반환해
// 호출자가 현재 상태를 다시 확인하게 한다.
func (e *RTMPEgress) transition(change egressTransition) bool {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()

	current := e.status.Phase
	next, allowed := nextEgressPhase(current, change.event)
	if !allowed {
		return false
	}
	if current == EgressPhaseStopped && change.event == egressTransitionStop {
		return false
	}

	now := time.Now().UTC()
	e.status.Phase = next
	e.status.UpdatedAt = now

	switch change.event {
	case egressTransitionProfileReady:
		e.status.Width = change.width
		e.status.Height = change.height
		e.status.FPS = change.fps
		if e.status.StartedAt == nil {
			e.status.StartedAt = &now
		}
	case egressTransitionPause:
		e.status.PausedAt = &now
	case egressTransitionReconnect:
		e.status.ReconnectAttempts = change.reconnectAttempts
		if change.cause != nil {
			message := e.sanitizeError(change.cause)
			e.status.LastError = &message
		}
	case egressTransitionStop:
		if e.status.StoppedAt == nil {
			e.status.StoppedAt = &now
		}
		if change.reason != "" && e.status.StopReason == nil {
			reason := change.reason
			e.status.StopReason = &reason
		}
		if change.cause != nil {
			message := e.sanitizeError(change.cause)
			e.status.LastError = &message
		}
	}
	return true
}

// nextEgressPhase는 상태 전이표 자체다. 이 함수에는 상태 스냅샷 이외의
// 부수 효과가 없으므로, 허용되지 않는 전이를 단위 테스트로 고정할 수 있다.
func nextEgressPhase(current EgressPhase, event egressTransitionEvent) (EgressPhase, bool) {
	switch event {
	case egressTransitionProfileReady:
		switch current {
		case EgressPhaseIdle, EgressPhaseReconfiguring, EgressPhaseReconnecting:
			return EgressPhaseStreaming, true
		case EgressPhasePausedReconfiguring, EgressPhasePausedReconnecting:
			return EgressPhasePaused, true
		}
	case egressTransitionPause:
		switch current {
		case EgressPhaseStreaming:
			return EgressPhasePaused, true
		case EgressPhaseReconfiguring:
			return EgressPhasePausedReconfiguring, true
		}
	case egressTransitionResume:
		if current == EgressPhasePaused {
			return EgressPhaseStreaming, true
		}
	case egressTransitionReconnect:
		switch current {
		case EgressPhaseIdle, EgressPhaseStreaming, EgressPhaseReconfiguring, EgressPhaseReconnecting:
			return EgressPhaseReconnecting, true
		case EgressPhasePaused, EgressPhasePausedReconfiguring, EgressPhasePausedReconnecting:
			return EgressPhasePausedReconnecting, true
		}
	case egressTransitionReconfigure:
		switch current {
		case EgressPhaseStreaming:
			return EgressPhaseReconfiguring, true
		case EgressPhasePaused:
			return EgressPhasePausedReconfiguring, true
		}
	case egressTransitionStop:
		return EgressPhaseStopped, true
	}
	return current, false
}

// setStreaming은 스폰 성공 후 profile_ready 전이를 기록한다. StartedAt은 최초
// 진입 시각을 보존한다(재연결·해상도 재기동으로 갱신하지 않는다).
func (e *RTMPEgress) setStreaming(width, height uint16, fps int) {
	e.transition(egressTransition{
		event:  egressTransitionProfileReady,
		width:  width,
		height: height,
		fps:    fps,
	})
}

// Pause는 RTMP egress를 종료하지 않고 취소 슬레이트·무음 오디오 상태로
// 전환한다. 재구성 중이면 새 출력 프로필이 확정된 직후 paused 상태가 되도록
// paused_reconfiguring을 기록한다.
func (e *RTMPEgress) Pause() bool {
	if !e.transition(egressTransition{event: egressTransitionPause}) {
		return false
	}
	if e.audio != nil {
		e.audio.SetMuted(true)
	}
	return true
}

// Resume은 paused 상태를 streaming 상태로 전환한다. PausedAt은 마지막 일시
// 중단 이력으로 유지한다.
func (e *RTMPEgress) Resume() bool {
	if !e.transition(egressTransition{event: egressTransitionResume}) {
		return false
	}
	if e.audio != nil {
		e.audio.SetMuted(false)
	}
	return true
}

// Stop은 egress를 즉시 stopped로 전이한다. 실제 FFmpeg 종료와 RTMP 연결
// 해제는 호출자가 egress context를 취소해 Run 고루틴이 수행한다. 두 동작을
// 분리하면 stop API와 세션 삭제가 재구성·재연결 완료를 기다리지 않아도 된다.
func (e *RTMPEgress) Stop() {
	e.transition(egressTransition{event: egressTransitionStop})
	e.clearCancellationSlate()
}

// StopWithReason은 egress 내부 복구 경로가 더 이상 자동 복구하지 않기로 했을
// 때 종료 사유까지 함께 기록한다. 먼저 stopped로 전이한 사건의 사유를 보존해
// 수동 stop·세션 종료와 뒤늦은 재연결 실패가 서로 덮어쓰지 않게 한다.
func (e *RTMPEgress) StopWithReason(reason EgressStopReason) {
	e.transition(egressTransition{event: egressTransitionStop, reason: reason})
	e.clearCancellationSlate()
}

// StopWithError는 종료 사유와 마지막 오류를 하나의 상태 전이로 기록한다.
// 재연결 예산 소진처럼 Run 고루틴이 스스로 종료하는 경우에 사용한다. 이미
// 수동 stop이 먼저 끝낸 egress라면 false를 돌려 종료 지표가 중복되지 않게 한다.
func (e *RTMPEgress) StopWithError(reason EgressStopReason, cause error) bool {
	if !e.transition(egressTransition{event: egressTransitionStop, reason: reason, cause: cause}) {
		return false
	}
	e.clearCancellationSlate()
	switch reason {
	case EgressStopReasonReconnectExhausted:
		e.metrics.IncEgressReconnectExhausted()
	case EgressStopReasonReconnectInputTimeout:
		e.metrics.IncEgressReconnectInputTimeout()
	}
	return true
}

// noteReconnect는 송출 실패로 백오프 대기에 들어감을 기록한다.
func (e *RTMPEgress) noteReconnect(cause error, attempts int) {
	e.transition(egressTransition{event: egressTransitionReconnect, cause: cause, reconnectAttempts: attempts})
}

// setReconnectAttempts는 실제 재시도용 FFmpeg 프로세스를 시작한 직후 호출한다.
// 오류 발생 시점과 프로세스 시작 시점이 다르므로 전이표를 다시 통과하지 않고
// 재연결 상태의 관측값만 갱신한다.
func (e *RTMPEgress) setReconnectAttempts(attempts int) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	if e.status.Phase == EgressPhaseStopped {
		return
	}
	e.status.ReconnectAttempts = attempts
	e.status.UpdatedAt = time.Now().UTC()
}

// resetReconnectStatus는 한 출력 세대가 충분히 안정적으로 송출된 뒤 이전
// 장애 구간의 API 표시를 지운다. Prometheus 누적 카운터는 여기서 건드리지 않는다.
func (e *RTMPEgress) resetReconnectStatus() {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	if e.status.Phase == EgressPhaseStopped {
		return
	}
	e.status.ReconnectAttempts = 0
	e.status.LastError = nil
	e.status.UpdatedAt = time.Now().UTC()
}

// sanitizeError는 StreamState와 로그에 보관할 오류에서 출력 URL의 스트림 키를
// 제거한다. RTMP endpoint가 오류 문구를 그대로 되돌려도 API 응답으로 비밀값이
// 나가지 않게 한다.
func (e *RTMPEgress) sanitizeError(cause error) string {
	return strings.ReplaceAll(cause.Error(), e.outputURL, maskStreamKey(e.outputURL))
}

// beginReconfiguring은 입력 프레임의 해상도가 바뀌어 새 출력 프로필을
// 측정해야 함을 기록한다. 취소 상태였다면 취소 의도를 보존한다.
func (e *RTMPEgress) beginReconfiguring() {
	e.transition(egressTransition{event: egressTransitionReconfigure})
	e.clearCancellationSlate()
}

// setStopped는 Run 종료를 기록한다.
func (e *RTMPEgress) setStopped() {
	e.Stop()
}

// videoBitrateFor는 측정한 소스 해상도에 맞는 x264 목표 bitrate를 고른다.
// 720p 이하는 기존 2500k를 유지하고, 그보다 크면 FHD bitrate를 사용한다.
// 비어 있지 않은 EGRESS_VIDEO_BITRATE는 해상도와 무관하게 우선한다.
func (e *RTMPEgress) videoBitrateFor(width, height uint16) string {
	if e.bitrateOverride != "" {
		return e.bitrateOverride
	}
	// 출력 해상도를 고정했다면 측정된 입력이 아니라 그 해상도를 기준으로 고른다.
	if pinnedWidth, pinnedHeight, ok := parseVideoSize(e.videoSize); ok {
		width, height = pinnedWidth, pinnedHeight
	}
	if int(width)*int(height) > 1280*720 {
		return egressVideoBitrateFHD
	}
	return egressVideoBitrate
}

// Enqueue는 호출자를 막지 않고 처리된 프레임을 egress에 전달한다. 큐가 가득 차면
// 가장 오래된 프레임을 먼저 버린다.
func (e *RTMPEgress) Enqueue(item frame) {
	select {
	case e.input <- item:
		return
	default:
	}
	select {
	case <-e.input:
		e.metrics.IncEgressFrameDropped()
	default:
	}
	select {
	case e.input <- item:
	default:
		e.metrics.IncEgressFrameDropped()
	}
}

// Run은 단일 writer 고루틴을 실행한다. 소스 프레임률을 측정하고 FFmpeg 자식
// 프로세스를 생성해 프레임을 공급하며, 자식 프로세스 종료나 RTMP 쓰기 경로 실패
// 시 제한된 지수 backoff로 재연결한다. 연결이 끊긴 동안 들어오는 프레임은 버리고
// 버퍼링하지 않는다.
//
// egress가 세션 수명으로 살게 되면서(#84) 트랙 교체로 프레임 해상도가
// 바뀌어도 이 고루틴이 계속 담당한다: 바깥 루프가 해상도 한 세대를,
// 안쪽 루프가 그 해상도에서의 스폰·재연결을 관리한다. 해상도가 바뀐
// 프레임을 만나면 백오프 없이 바깥 루프로 나가 새 해상도로 재측정한다.
func (e *RTMPEgress) Run(ctx context.Context) {
	defer e.setStopped()
	budget := newReconnectBudget(e.reconnectPolicy)
	var seed []frame
	var inputDeadline time.Time
	for ctx.Err() == nil {
		startup, result := e.collectStartupFrames(ctx, seed, inputDeadline)
		if result != startupFramesReady {
			if result == startupFramesTimedOut && ctx.Err() == nil {
				e.logger.Warn("RTMP egress input profile collection timed out", "error", errReconnectInputTimeout)
				e.StopWithError(EgressStopReasonReconnectInputTimeout, errReconnectInputTimeout)
			}
			return
		}
		seed = nil
		inputDeadline = time.Time{}
		width, height := startup[0].width, startup[0].height
		fps := measureFPS(startup)
		e.logger.Info("starting RTMP egress",
			"url", maskStreamKey(e.outputURL), "width", width, "height", height, "fps", fps,
			"video_bitrate", e.videoBitrateFor(width, height))

		pending := startup
		for ctx.Err() == nil {
			process, err := e.startProcess(ctx, width, height, fps)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				e.logger.Error("start RTMP egress FFmpeg failed", "error", e.sanitizeError(err))
				if !e.waitForReconnect(ctx, &budget, err) {
					return
				}
				continue
			}
			e.setStreaming(width, height, fps)
			_, mismatch, err := e.writeFrames(ctx, process, pending, width, height, fps, func() {
				// 300프레임을 끊김 없이 넘긴 뒤에야 이전 장애 구간을 끝낸다.
				// writeFrames와 같은 Run 고루틴에서 실행되므로 budget은 별도 잠금 없이
				// 안전하게 초기화할 수 있다.
				budget.reset()
				e.resetReconnectStatus()
			})
			pending = nil
			// 자식 프로세스가 pipe:3에서 EOF를 받아 종료하게 하고, 다음 spawn이
			// 새 Ogg 스트림을 연결할 수 있도록 해제한다.
			if e.audio != nil {
				e.audio.Detach(e.audioWriteEnd)
			}
			process.close()
			if ctx.Err() != nil {
				return
			}
			if mismatch != nil {
				// 트랙 교체로 해상도가 바뀌었다. 이 프레임을 시드로 새 프로필을
				// 측정한다. 새 프레임을 받는 시간도 현재 재연결 예산을 넘지 않게
				// 제한해, 입력이 끊긴 채 영구 대기하지 않는다.
				e.beginReconfiguring()
				e.logger.Info("video resolution changed; restarting RTMP egress",
					"old_width", width, "old_height", height,
					"new_width", mismatch.width, "new_height", mismatch.height)
				seed = []frame{*mismatch}
				inputDeadline = time.Now().UTC().Add(e.reconnectPolicy.maxElapsed)
				if deadline := budget.deadline(); !deadline.IsZero() && deadline.Before(inputDeadline) {
					inputDeadline = deadline
				}
				break
			}
			if err == nil {
				err = errors.New("RTMP egress stopped without a write error")
			}
			if !e.waitForReconnect(ctx, &budget, err) {
				return
			}
		}
	}
}

// collectStartupFrames는 fps·해상도 측정에 쓸 프레임을 모은다. seed는 해상도
// 재기동 시 이월되는 새 해상도의 첫 프레임으로, 수집 목표 수에 포함된다.
// deadline이 0이면 첫 시작처럼 시간 제한 없이 기다린다. 재구성·복구 경로는
// 유한한 deadline을 전달해 입력 단절로 Run 고루틴이 영구 대기하지 않게 한다.
type startupFramesResult uint8

const (
	startupFramesReady startupFramesResult = iota
	startupFramesCanceled
	startupFramesTimedOut
)

func (e *RTMPEgress) collectStartupFrames(ctx context.Context, seed []frame, deadline time.Time) ([]frame, startupFramesResult) {
	startup := make([]frame, 0, egressMeasureFrames)
	startup = append(startup, seed...)

	var deadlineC <-chan time.Time
	var timer *time.Timer
	if !deadline.IsZero() {
		until := time.Until(deadline)
		if until <= 0 {
			return nil, startupFramesTimedOut
		}
		timer = time.NewTimer(until)
		defer timer.Stop()
		deadlineC = timer.C
	}

	for len(startup) < egressMeasureFrames {
		select {
		case <-ctx.Done():
			return nil, startupFramesCanceled
		case <-deadlineC:
			return nil, startupFramesTimedOut
		case item := <-e.input:
			startup = append(startup, item)
		}
	}
	return startup, startupFramesReady
}

// writeFrames는 프레임을 FFmpeg에 공급한다. 스폰 해상도와 다른 프레임을
// 만나면 그 프레임을 반환하고 즉시 중단한다 — 해상도가 다른 데이터를 밀면
// 인코더가 어차피 죽고, 죽은 뒤에는 재기동 기준 해상도를 알 수 없기 때문에
// 죽기 전에 감지해 재측정 시드로 넘기는 것이다.
func (e *RTMPEgress) writeFrames(ctx context.Context, process *ffmpegProcess, pending []frame, width, height uint16, fps int, onStable func()) (int, *frame, error) {
	written := 0
	stable := false
	noteWritten := func() {
		written++
		if !stable && written >= egressStableFrames {
			stable = true
			onStable()
		}
	}
	writeCamera := func(item frame) (*frame, error) {
		if resolutionChanged(item, width, height) {
			return &item, nil
		}
		if !e.validFrame(item) {
			e.metrics.IncEgressFrameDropped()
			return nil, nil
		}
		if _, err := process.stdin.Write(item.data); err != nil {
			return nil, err
		}
		noteWritten()
		e.latency.observe(item.ingestAt)
		return nil, nil
	}
	writeSlate := func() error {
		slate, err := e.cancellationSlate(width, height)
		if err != nil {
			return err
		}
		if _, err := process.stdin.Write(slate.data); err != nil {
			return err
		}
		noteWritten()
		return nil
	}
	for _, item := range pending {
		if e.isPaused() {
			if resolutionChanged(item, width, height) {
				return written, &item, nil
			}
			continue
		}
		if mismatch, err := writeCamera(item); mismatch != nil || err != nil {
			return written, mismatch, err
		}
	}
	if fps < 1 {
		fps = egressDefaultFPS
	}
	slateTicker := time.NewTicker(time.Second / time.Duration(fps))
	defer slateTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return written, nil, ctx.Err()
		case item := <-e.input:
			if e.isPaused() {
				if resolutionChanged(item, width, height) {
					return written, &item, nil
				}
				continue
			}
			if mismatch, err := writeCamera(item); mismatch != nil || err != nil {
				return written, mismatch, err
			}
		case <-slateTicker.C:
			if e.isPaused() {
				if err := writeSlate(); err != nil {
					return written, nil, err
				}
			}
		}
	}
}

// cancellationSlate는 이 egress의 현재 출력 규격에 맞는 슬레이트 한 장만
// 보관한다. pause 중 매 프레임마다 PNG를 다시 변환하지 않으면서도, 다른 세션이
// 사용한 임의의 해상도가 이 egress 또는 전역 메모리에 남지 않게 한다.
func (e *RTMPEgress) cancellationSlate(width, height uint16) (frame, error) {
	e.slateMu.Lock()
	defer e.slateMu.Unlock()

	if e.slate != nil && e.slate.width == width && e.slate.height == height && e.slate.format == e.wireFormat {
		return e.slate.frame, nil
	}

	generated, err := cancellationSlateFrame(width, height, e.wireFormat)
	if err != nil {
		return frame{}, err
	}
	e.slate = &cachedCancellationSlate{
		width:  width,
		height: height,
		format: e.wireFormat,
		frame:  generated,
	}
	return generated, nil
}

// clearCancellationSlate는 출력 규격이 바뀌거나 egress가 종료될 때 이전
// 슬레이트의 참조를 끊는다. 큰 raw YUV 프레임도 이후 GC 대상이 된다.
func (e *RTMPEgress) clearCancellationSlate() {
	e.slateMu.Lock()
	e.slate = nil
	e.slateMu.Unlock()
}

func (e *RTMPEgress) isPaused() bool {
	return e.Status().Phase == EgressPhasePaused
}

// resolutionChanged는 프레임 해상도가 현재 스폰 해상도와 다른지 판정한다.
// 해상도 정보가 없는 프레임(0값)은 비교에서 제외해 기존 검증(validFrame)
// 경로에 맡긴다 — 0×0을 기준 삼아 재기동을 반복하는 것을 막는 가드다.
func resolutionChanged(item frame, width, height uint16) bool {
	if item.width == 0 || item.height == 0 {
		return false
	}
	return item.width != width || item.height != height
}

func (e *RTMPEgress) validFrame(item frame) bool {
	if e.wireFormat == config.WireFormatRaw {
		return len(item.data) == rawFrameSize(item.width, item.height)
	}
	return isJPEG(item.data)
}

// waitForReconnect은 오류를 현재 장애 구간에 기록하고, 다음 실제 FFmpeg 시작을
// 허용하는 경우에만 true를 반환한다. 재시도 횟수와 총 시간 중 하나라도 소진되면
// 같은 오류를 종료 사유와 함께 보존하고 Run을 끝내게 한다.
func (e *RTMPEgress) waitForReconnect(ctx context.Context, budget *reconnectBudget, cause error) bool {
	now := time.Now().UTC()
	budget.noteFailure(now)
	e.noteReconnect(cause, budget.attempts)

	delay, allowed := budget.nextDelay(now)
	if !allowed {
		if ctx.Err() == nil {
			e.StopWithError(EgressStopReasonReconnectExhausted, cause)
		}
		return false
	}

	e.logger.Warn("RTMP egress disconnected; reconnecting",
		"error", e.sanitizeError(cause),
		"reconnect_attempts", budget.attempts,
		"retry_in", delay)
	if !e.waitBackoff(ctx, delay) {
		return false
	}

	now = time.Now().UTC()
	if !budget.beginAttempt(now) {
		if ctx.Err() == nil {
			e.StopWithError(EgressStopReasonReconnectExhausted, cause)
		}
		return false
	}
	e.setReconnectAttempts(budget.attempts)
	e.metrics.IncEgressReconnect()
	return true
}

// waitBackoff는 주어진 재연결 대기 시간 동안 프레임을 비우며(버리며) 대기한다.
// 재시도 간격의 계산은 reconnectBudget이 담당하므로 이 함수는 대기·취소만 맡는다.
func (e *RTMPEgress) waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-e.input:
			e.metrics.IncEgressFrameDropped()
		case <-timer.C:
			return true
		}
	}
}

// videoFilter는 FFmpeg -vf 체인을 만든다.
//
// EGRESS_VIDEO_SIZE가 설정돼 있으면 입력 해상도와 무관하게 그 크기로 맞춘다
// (종횡비 유지 후 레터박스). WebRTC 퍼블리셔는 대역폭 추정에 따라 스트림
// 도중 해상도를 바꾸는데, H.264 경로에서는 그 변화가 frame 메타데이터에
// 반영되지 않아(#122) resolutionChanged가 감지하지 못한다. 그 결과 FFmpeg가
// 첫 프레임 해상도로 libx264를 초기화한 뒤 이후 프레임을 조용히 다운스케일해
// 송출 화질이 시작 시점 해상도에 묶였다(#121). 출력 해상도를 고정하면 입력
// 변동을 필터가 흡수하므로 RTMP 재연결 없이 화질이 유지된다.
//
// MJPEG 입력은 full-range yuvj420p로 디코딩되므로 FLV 스트림이 일반
// yuv420p를 담도록 limited range로 압축한다.
func (e *RTMPEgress) videoFilter() string {
	var stages []string
	if width, height, ok := parseVideoSize(e.videoSize); ok {
		stages = append(stages,
			fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", width, height),
			fmt.Sprintf("pad=%d:%d:-1:-1", width, height))
	}
	if e.wireFormat != config.WireFormatRaw {
		stages = append(stages, "scale=out_range=tv")
	}
	return strings.Join(stages, ",")
}

// parseVideoSize는 "1280x720" 형식을 파싱한다. 형식에 맞지 않으면 ok가 false다
// (설정 검증은 config.Validate가 담당하므로 여기서는 조용히 무시한다).
func parseVideoSize(value string) (uint16, uint16, bool) {
	rawWidth, rawHeight, found := strings.Cut(strings.TrimSpace(value), "x")
	if !found {
		return 0, 0, false
	}
	width, err := strconv.Atoi(rawWidth)
	if err != nil || width <= 0 || width > math.MaxUint16 {
		return 0, 0, false
	}
	height, err := strconv.Atoi(rawHeight)
	if err != nil || height <= 0 || height > math.MaxUint16 {
		return 0, 0, false
	}
	return uint16(width), uint16(height), true
}

func (e *RTMPEgress) start(ctx context.Context, width, height uint16, fps int) (*ffmpegProcess, error) {
	useAudio := e.audio != nil && e.audio.PacketSeen()
	arguments := []string{
		"-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:2", "-y",
		"-thread_queue_size", "512", "-use_wallclock_as_timestamps", "1",
	}
	if useAudio {
		// FFmpeg는 입력을 순차적으로 열며, 기본 probesize(5MB)에서는 pipe:3을
		// 열기 전 영상 수 초를 읽는다. 그 사이 마이크 mux가 늦어지거나 pipe:3을
		// 열기도 전에 자식 프로세스가 종료될 수 있다. codec과 프레임률은 이미
		// 강제하므로 영상 probe를 제한해 pipe:3을 신속히 열게 한다. 무음 경로의
		// 타이밍은 건드리지 않도록 오디오 경로에만 적용한다.
		arguments = append(arguments, "-analyzeduration", "0", "-probesize", "1000000")
	}
	if e.wireFormat == config.WireFormatRaw {
		arguments = append(arguments,
			"-f", "rawvideo", "-pixel_format", "yuv420p",
			"-video_size", fmt.Sprintf("%dx%d", width, height),
			"-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		)
	} else {
		arguments = append(arguments,
			"-f", "image2pipe", "-vcodec", "mjpeg",
			"-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		)
	}
	// 두 번째 입력은 송출자의 Opus 마이크가 흐를 때 pipe:3을 사용하고, 그렇지
	// 않으면 생성한 무음을 사용한다. 오디오 입력은 자체 Opus 타임라인을 유지하도록
	// -use_wallclock_as_timestamps를 의도적으로 쓰지 않는다(위 영상 입력에서
	// 해당 플래그를 사용했다). A/V 동기화는 4단계에서 처리한다.
	var extraFiles []*os.File
	var audioWriteEnd *os.File
	if useAudio {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("create audio egress pipe: %w", err)
		}
		extraFiles = []*os.File{readEnd}
		audioWriteEnd = writeEnd
		arguments = append(arguments, "-thread_queue_size", "512")
		if e.audioOffset != 0 {
			// -itsoffset은 오디오 입력 timestamp를 blur 지연 영상에 맞춘다.
			// 오디오 -i보다 앞에 와야 한다.
			arguments = append(arguments, "-itsoffset", fmt.Sprintf("%.3f", e.audioOffset.Seconds()))
		}
		arguments = append(arguments,
			"-f", "ogg", "-i", "pipe:3",
			"-map", "0:v", "-map", "1:a",
		)
	} else {
		arguments = append(arguments,
			"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
			"-map", "0:v", "-map", "1:a",
		)
	}
	if filter := e.videoFilter(); filter != "" {
		arguments = append(arguments, "-vf", filter)
	}
	videoBitrate := e.videoBitrateFor(width, height)
	arguments = append(arguments,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-pix_fmt", "yuv420p", "-profile:v", "main",
		"-b:v", videoBitrate, "-maxrate", videoBitrate, "-bufsize", videoBitrate,
		"-g", strconv.Itoa(fps*2), "-bf", "0",
		"-r", strconv.Itoa(fps), "-fps_mode", "cfr",
		"-c:a", "aac", "-b:a", egressAudioBitrate, "-ar", "44100", "-ac", "2",
	)
	if useAudio {
		// 스트림을 종료하지 않고 FFmpeg가 영상 clock을 기준으로 Opus clock drift와
		// DTX 공백을 흡수하게 한다.
		arguments = append(arguments, "-af", "aresample=async=1")
	} else {
		// 무음은 영상 길이를 따르며, 영상이 끝나면 스트림을 종료한다.
		arguments = append(arguments, "-shortest")
	}
	arguments = append(arguments, "-f", "flv", e.outputURL)

	process, err := e.transcoder.startFFmpeg(ctx, "egress", e.handleStderrLine, extraFiles, arguments...)
	if err != nil {
		if audioWriteEnd != nil {
			_ = audioWriteEnd.Close()
		}
		return nil, fmt.Errorf("start FFmpeg RTMP egress: %w", err)
	}
	e.audioWriteEnd = audioWriteEnd
	if audioWriteEnd != nil {
		if err := e.audio.Attach(audioWriteEnd); err != nil {
			_ = audioWriteEnd.Close()
			process.close()
			return nil, fmt.Errorf("attach audio egress pipe: %w", err)
		}
		e.logger.Info("audio egress pipe attached", "channels", e.audio.Channels(), "itsoffset_ms", e.audioOffset.Milliseconds())
	}
	return process, nil
}

// handleStderrLine은 FFmpeg의 -progress key=value 스트림(-fps_mode cfr가 만든
// dup/drop 프레임 카운터 포함)과 실제 오류를 구분한다.
func (e *RTMPEgress) handleStderrLine(line string) {
	key, value, found := strings.Cut(line, "=")
	if found && isProgressKey(key) {
		switch key {
		case "dup_frames":
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				e.metrics.SetEgressDupFrames(parsed)
			}
		case "drop_frames":
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				e.metrics.SetEgressDropFrames(parsed)
			}
		}
		return
	}
	// FFmpeg는 스트림 키가 포함된 출력 URL을 자체 오류 메시지에 그대로 넣으므로,
	// 로그에 도달하기 전에 마스킹한다.
	e.logger.Warn("FFmpeg reported an error", "message", strings.ReplaceAll(line, e.outputURL, maskStreamKey(e.outputURL)))
}

func isProgressKey(key string) bool {
	switch key {
	case "frame", "fps", "bitrate", "total_size", "out_time_us", "out_time_ms",
		"out_time", "dup_frames", "drop_frames", "speed", "progress":
		return true
	}
	return strings.HasPrefix(key, "stream_")
}

// measureFPS는 버퍼링한 시작 프레임의 RTP timestamp(90kHz clock)로 소스
// 프레임률을 계산한다. uint32 뺄셈으로 timestamp wrap-around를 처리한다.
//
// 이 값은 -r/-fps_mode cfr로 세션 내내 고정되므로 한 번 낮게 잡히면 이후
// 정상 속도로 들어오는 프레임을 계속 버린다. 그런데 측정 구간인 시작
// 프레임 몇 장은 하필 WebRTC 대역폭 추정이 가장 낮은 때라 낮게 잡히기
// 쉽다. 그래서 추정 방식을 두 번 좁혔다.
//
// 먼저 전체 구간을 프레임 수로 나누는 방식은 공백 몇 개가 분모를 부풀려
// 무너지므로 인접 간격의 통계로 바꿨다(#127에서 프로덕션 실측). 다음으로
// 중앙값도 초반 구간 전체가 균일하게 느리면 통째로 포획된다는 것이
// 드러났다(#134). 측정 창이 egressMeasureFrames장이라 간격은 그보다 하나
// 적고, 중앙값은 그 절반 지점을 고른다 — 느린 구간이 간격의 절반만
// 만들면 넘어간다. 느린 구간은 시간당 프레임을 적게 만들므로, 아주
// 느릴 때보다 어중간하게 느릴 때(10~20fps) 오히려 쉽게 넘어간다.
//
// 그래서 하위 백분위를 쓴다. 드롭은 간격을 늘리기만 하고 줄이지 않으므로
// 가장 짧은 축이 소스의 실제 능력을 가장 정직하게 반영한다. 간격이 고르게
// 넓은 진짜 저프레임 소스는 종전과 같이 그대로 잡는다 — 그 경우 짧은
// 축도 함께 넓기 때문이다.
func measureFPS(frames []frame) int {
	if len(frames) < 2 {
		return egressDefaultFPS
	}
	intervals := make([]uint32, 0, len(frames)-1)
	for i := 1; i < len(frames); i++ {
		// uint32 뺄셈이 wrap-around를 흡수한다. 같은 timestamp를 가진
		// 프레임은 간격 표본이 아니므로 제외한다.
		if delta := frames[i].timestamp - frames[i-1].timestamp; delta > 0 {
			intervals = append(intervals, delta)
		}
	}
	if len(intervals) == 0 {
		return egressDefaultFPS
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i] < intervals[j] })
	// 하위 10% 지점. 최솟값을 그대로 쓰지 않는 것은 timestamp 지터나 재정렬로
	// 한두 칸이 비정상적으로 짧게 찍히는 경우를 견디기 위해서다. 표본이 적으면
	// 자연히 최솟값으로 수렴한다.
	shortest := intervals[len(intervals)/10]
	fps := int(math.Round(videoClockRate / float64(shortest)))
	if fps < 1 || fps > 120 {
		return egressDefaultFPS
	}
	return fps
}

// maskStreamKey는 RTMP URL의 마지막 경로 조각(스트림 키)을 숨겨 로그에 남지
// 않게 한다.
func maskStreamKey(url string) string {
	index := strings.LastIndex(url, "/")
	if index < 0 || index == len(url)-1 {
		return url
	}
	return url[:index+1] + "****"
}
