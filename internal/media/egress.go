package media

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
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
	// egressStableFrames는 이후 실패 시 재연결 backoff를 최솟값으로 되돌리려면
	// 프로세스가 받아야 하는 프레임 수다.
	egressStableFrames = 300
)

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
	event  egressTransitionEvent
	width  uint16
	height uint16
	fps    int
	cause  error
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
	input           chan frame

	statusMu sync.Mutex
	status   EgressStatus
}

func NewRTMPEgress(path string, logger *slog.Logger, registry *metrics.Registry, options TranscoderOptions, outputURL string, audio *AudioPipe, latencyLog bool, audioOffset time.Duration, videoBitrate string) *RTMPEgress {
	if options.WireFormat == "" {
		options.WireFormat = config.WireFormatJPEG
	}
	egressLogger := logger.With("ffmpeg_role", "egress")
	return &RTMPEgress{
		transcoder:      NewFFmpegTranscoder(path, logger, registry, options),
		logger:          egressLogger,
		metrics:         registry,
		wireFormat:      options.WireFormat,
		outputURL:       outputURL,
		audio:           audio,
		latency:         newLatencyTracker(egressLogger, latencyLog),
		audioOffset:     audioOffset,
		bitrateOverride: videoBitrate,
		input:           make(chan frame, egressQueueSize),
		status: EgressStatus{
			Phase:     EgressPhaseIdle,
			TargetURL: maskStreamKey(outputURL),
			UpdatedAt: time.Now().UTC(),
		},
	}
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
		return true
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
		e.status.ReconnectAttempts++
		if change.cause != nil {
			message := change.cause.Error()
			e.status.LastError = &message
		}
	case egressTransitionStop:
		if e.status.StoppedAt == nil {
			e.status.StoppedAt = &now
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
}

// noteReconnect는 송출 실패로 백오프 대기에 들어감을 기록한다.
func (e *RTMPEgress) noteReconnect(cause error) {
	e.transition(egressTransition{event: egressTransitionReconnect, cause: cause})
}

// beginReconfiguring은 입력 프레임의 해상도가 바뀌어 새 출력 프로필을
// 측정해야 함을 기록한다. 취소 상태였다면 취소 의도를 보존한다.
func (e *RTMPEgress) beginReconfiguring() {
	e.transition(egressTransition{event: egressTransitionReconfigure})
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
// 시 지수 backoff로 재연결한다. 연결이 끊긴 동안 들어오는 프레임은 버리고
// 버퍼링하지 않는다.
//
// egress가 세션 수명으로 살게 되면서(#84) 트랙 교체로 프레임 해상도가
// 바뀌어도 이 고루틴이 계속 담당한다: 바깥 루프가 해상도 한 세대를,
// 안쪽 루프가 그 해상도에서의 스폰·재연결을 관리한다. 해상도가 바뀐
// 프레임을 만나면 백오프 없이 바깥 루프로 나가 새 해상도로 재측정한다.
func (e *RTMPEgress) Run(ctx context.Context) {
	defer e.setStopped()
	backoff := egressBackoffMin
	var seed []frame
	for ctx.Err() == nil {
		startup, ok := e.collectStartupFrames(ctx, seed)
		if !ok {
			return
		}
		seed = nil
		width, height := startup[0].width, startup[0].height
		fps := measureFPS(startup)
		e.logger.Info("starting RTMP egress",
			"url", maskStreamKey(e.outputURL), "width", width, "height", height, "fps", fps,
			"video_bitrate", e.videoBitrateFor(width, height))

		pending := startup
		for ctx.Err() == nil {
			process, err := e.start(ctx, width, height, fps)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				e.logger.Error("start RTMP egress FFmpeg failed", "error", err)
				e.noteReconnect(err)
				e.waitBackoff(ctx, &backoff)
				continue
			}
			e.setStreaming(width, height, fps)
			written, mismatch, err := e.writeFrames(ctx, process, pending, width, height, fps)
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
				// 트랙 교체로 해상도가 바뀌었다. 송출 실패가 아니므로 백오프와
				// 재연결 집계 없이, 이 프레임을 시드로 즉시 재측정에 들어간다.
				e.beginReconfiguring()
				e.logger.Info("video resolution changed; restarting RTMP egress",
					"old_width", width, "old_height", height,
					"new_width", mismatch.width, "new_height", mismatch.height)
				seed = []frame{*mismatch}
				break
			}
			if written >= egressStableFrames {
				backoff = egressBackoffMin
			}
			e.metrics.IncEgressReconnect()
			e.noteReconnect(err)
			e.logger.Warn("RTMP egress disconnected; reconnecting",
				"error", err, "frames_written", written, "backoff", backoff)
			e.waitBackoff(ctx, &backoff)
		}
	}
}

// collectStartupFrames는 fps·해상도 측정에 쓸 프레임을 모은다. seed는 해상도
// 재기동 시 이월되는 새 해상도의 첫 프레임으로, 수집 목표 수에 포함된다.
func (e *RTMPEgress) collectStartupFrames(ctx context.Context, seed []frame) ([]frame, bool) {
	startup := make([]frame, 0, egressMeasureFrames)
	startup = append(startup, seed...)
	for len(startup) < egressMeasureFrames {
		select {
		case <-ctx.Done():
			return nil, false
		case item := <-e.input:
			startup = append(startup, item)
		}
	}
	return startup, true
}

// writeFrames는 프레임을 FFmpeg에 공급한다. 스폰 해상도와 다른 프레임을
// 만나면 그 프레임을 반환하고 즉시 중단한다 — 해상도가 다른 데이터를 밀면
// 인코더가 어차피 죽고, 죽은 뒤에는 재기동 기준 해상도를 알 수 없기 때문에
// 죽기 전에 감지해 재측정 시드로 넘기는 것이다.
func (e *RTMPEgress) writeFrames(ctx context.Context, process *ffmpegProcess, pending []frame, width, height uint16, fps int) (int, *frame, error) {
	written := 0
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
		written++
		e.latency.observe(item.ingestAt)
		return nil, nil
	}
	writeSlate := func() error {
		slate, err := cancellationSlateFrame(width, height, e.wireFormat)
		if err != nil {
			return err
		}
		if _, err := process.stdin.Write(slate.data); err != nil {
			return err
		}
		written++
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

// waitBackoff는 현재 backoff 동안 프레임을 비우며(버리며) 대기한다. 프로세스가
// 돌아왔을 때 큐에 최신 프레임이 남게 하며, 이후 backoff를 상한까지 두 배로 늘린다.
func (e *RTMPEgress) waitBackoff(ctx context.Context, backoff *time.Duration) {
	timer := time.NewTimer(*backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.input:
			e.metrics.IncEgressFrameDropped()
		case <-timer.C:
			*backoff *= 2
			if *backoff > egressBackoffMax {
				*backoff = egressBackoffMax
			}
			return
		}
	}
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
	if e.wireFormat != config.WireFormatRaw {
		// MJPEG는 full-range yuvj420p로 디코딩된다. FLV 스트림이 일반 yuv420p를
		// 담도록 limited range로 압축한다.
		arguments = append(arguments, "-vf", "scale=out_range=tv")
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

// measureFPS는 버퍼링한 시작 프레임의 RTP timestamp 범위(90kHz clock)로 소스
// 프레임률을 계산한다. uint32 뺄셈으로 timestamp wrap-around를 처리한다.
func measureFPS(frames []frame) int {
	if len(frames) < 2 {
		return egressDefaultFPS
	}
	delta := frames[len(frames)-1].timestamp - frames[0].timestamp
	if delta == 0 {
		return egressDefaultFPS
	}
	fps := int(math.Round(float64(len(frames)-1) * videoClockRate / float64(delta)))
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
