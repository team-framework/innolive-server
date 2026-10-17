package media

import (
	"context"
	"errors"
	"time"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/config"
)

// aiTask는 처리 고루틴이 AI로 보냈거나, 보내지 않고 결과가 정해진 프레임 하나다.
// pending이 nil이면 output·processed·err가 결과다.
type aiTask struct {
	output    []byte
	processed bool
	err       error

	pending   *ai.Pending
	stream    PipelinedAIStream
	frame     []byte
	timestamp int64
	width     uint16
	height    uint16
	probe     bool
	epoch     uint64
	startedAt time.Time
}

// SetAIInflight는 세션 하나가 AI 응답을 기다리며 보내 둘 프레임 수를 정한다. 1이면
// 한 장씩 보내고 받는다. 파이프라인을 띄우기 전에 부른다.
func (p *Processor) SetAIInflight(count int) {
	if count < 1 {
		count = 1
	}
	if count > MaxAIInflight {
		count = MaxAIInflight
	}
	p.aiInflight = count
}

// pipelinedAIInflight는 프레임을 여러 장 보내 둘 수 있으면 그 수를, 아니면 1을 돌려준다.
func (p *Processor) pipelinedAIInflight() int {
	if p.mode != config.PrivacyModeReal || p.aiInflight <= 1 {
		return 1
	}
	if _, ok := p.ai.(PipelinedAIStream); !ok {
		return 1
	}
	return p.aiInflight
}

// beginAIInput은 ProcessIfAIInputEnabled의 앞 절반이다. AI로 보낼 프레임은 응답을
// 기다리지 않고 보낸다. 결과는 finishAIInput으로 보낸 순서대로 받는다.
func (p *Processor) beginAIInput(ctx context.Context, frame []byte, timestamp int64, width, height uint16) aiTask {
	p.aiMu.RLock()
	defer p.aiMu.RUnlock()
	done := func(output []byte, err error) aiTask {
		return aiTask{output: output, processed: true, err: err}
	}
	if p.mode == config.PrivacyModeReal && p.aiInputPaused {
		return aiTask{}
	}
	if p.mode == config.PrivacyModeReal && p.screenBlurEnabled {
		return done(p.screenBlur(frame, width, height))
	}
	if p.mode == config.PrivacyModeReal && !p.anonymizationEnabled {
		return done(frame, nil)
	}
	stream, ok := p.ai.(PipelinedAIStream)
	if p.mode != config.PrivacyModeReal || !ok {
		return done(p.process(ctx, frame, timestamp, width, height))
	}

	probe := false
	if p.fallback.Load() {
		if !p.probeDue() {
			return done(p.serveBlackout(frame, width, height, nil))
		}
		p.lastProbeAt.Store(time.Now().UnixNano())
		probe = true
	}
	startedAt := time.Now()
	pending, err := stream.Send(frame, timestamp, width, height, p.aiPixFmt())
	if err != nil {
		_, err = p.acceptAIResponse(nil, err, timestamp, startedAt)
		p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt))
		return done(p.settleAIResult(frame, width, height, nil, err, probe))
	}
	return aiTask{
		pending:   &pending,
		stream:    stream,
		frame:     frame,
		timestamp: timestamp,
		width:     width,
		height:    height,
		probe:     probe,
		epoch:     p.aiEpoch.Load(),
		startedAt: startedAt,
	}
}

// finishAIInput은 beginAIInput으로 보낸 프레임의 결과를 받는다. processed가 false면
// AI 입력 중단 구간의 프레임이라 버린다.
func (p *Processor) finishAIInput(task aiTask) ([]byte, bool, error) {
	if task.pending == nil {
		return task.output, task.processed, task.err
	}
	response, err := task.stream.Recv(*task.pending)
	defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(task.startedAt)) }()
	if p.aiEpoch.Load() != task.epoch {
		// 응답을 기다리는 동안 입력 중단이 스트림을 닫았다. 중단 구간 프레임처럼 버린다.
		return nil, false, nil
	}
	if errors.Is(err, ai.ErrStreamReset) {
		// 앞서 보낸 프레임의 실패로 스트림이 닫혔다. 그 실패가 이미 실패 정책에
		// 반영됐으므로 이 프레임은 같은 정책으로 가리기만 한다.
		if p.failurePolicy == config.FailurePolicyFreeze {
			return nil, true, err
		}
		output, blackoutErr := p.serveBlackout(task.frame, task.width, task.height, err)
		return output, true, blackoutErr
	}
	output, err := p.acceptAIResponse(response, err, task.timestamp, task.startedAt)
	output, err = p.settleAIResult(task.frame, task.width, task.height, output, err, task.probe)
	return output, true, err
}

// discardAIInput은 결과를 받지 않을 프레임을 정리한다. 응답이 남은 스트림을 닫아
// 다음 프레임이 그 응답을 읽지 않게 한다.
func (p *Processor) discardAIInput(task aiTask) {
	if task.pending != nil {
		task.stream.Discard(*task.pending)
	}
}
