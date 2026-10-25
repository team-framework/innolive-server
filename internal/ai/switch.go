package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/protobuf/proto"
)

// DefaultWarmFrames는 새 워커가 세션 프레임을 오류 없이 돌려줘야 하는 횟수다. 이만큼
// 처리하면 다음 프레임부터 새 워커의 결과를 내보낸다.
const DefaultWarmFrames = 3

// idleSwitchAfter보다 오래 프레임이 없던 스트림은 예열 없이 바로 바꾼다. 방송 일시
// 정지 등으로 프레임이 흐르지 않으면 예열할 프레임도 없다. 다음 프레임이 새 워커에서
// 실패해도 Processor가 fail-closed로 막는다.
const idleSwitchAfter = 2 * time.Second

var (
	// ErrSwitchInProgress는 이미 다른 워커로 넘어가는 중인 스트림에 전환을 또 요청했을
	// 때다.
	ErrSwitchInProgress = errors.New("AI stream switch already in progress")
	// ErrSwitchClosed는 파이프라인이 끝나 스트림을 놓았을 때다.
	ErrSwitchClosed = errors.New("AI stream released")
	// ErrSwitchInterrupted는 예열 중에 AI 입력이 멈춰 전환을 그만뒀을 때다.
	ErrSwitchInterrupted = errors.New("AI stream switch interrupted by AI input close")
)

// switchLeg는 워커 하나에 연 세션 스트림과 그 워커의 배정이다.
type switchLeg struct {
	client  *Client
	stream  *Stream
	release func()
	// 아래는 예열 중인 후보에만 쓴다. warmed·ready는 SwitchableStream.mu가 보호한다.
	busy   atomic.Bool
	warmed int
	ready  bool
	done   chan error
}

func (l *switchLeg) discard() {
	l.stream.Close()
	l.release()
}

// SwitchableStream은 세션 하나의 AI 스트림을 파이프라인을 다시 띄우지 않고 다른
// 워커로 넘긴다(#515). 넘기는 동안 지금 워커가 프레임을 계속 처리하고, 새 워커는 같은
// 프레임의 사본으로 예열한다. 새 워커가 DefaultWarmFrames만큼 성공하면 다음 프레임부터
// 새 워커로 보낸다. 예열이 실패하면 지금 워커를 그대로 쓴다.
//
// 예열 프레임은 기다리지 않는다 — 출력 프레임은 언제나 지금 워커의 결과이므로 전환이
// 송출 지연을 늘리지 않는다.
type SwitchableStream struct {
	ctx        context.Context
	sessionID  string
	faceSwap   bool
	warmFrames int

	// processMu는 프레임 처리와 전환을 줄 세운다. 전환은 두 프레임 사이에서만 일어난다.
	processMu sync.Mutex

	mu            sync.Mutex
	current       *switchLeg
	candidate     *switchLeg
	lastProcessAt time.Time
	released      bool
}

// NewSwitchableStream은 client에 세션 스트림을 연다. release는 client 배정을 되돌리며
// 이 워커를 떠날 때 한 번 불린다.
func NewSwitchableStream(ctx context.Context, client *Client, release func(), sessionID string, faceSwap bool) *SwitchableStream {
	s := &SwitchableStream{ctx: ctx, sessionID: sessionID, faceSwap: faceSwap, warmFrames: DefaultWarmFrames}
	s.current = s.newLeg(client, release)
	return s
}

func (s *SwitchableStream) newLeg(client *Client, release func()) *switchLeg {
	return &switchLeg{
		client:  client,
		stream:  client.NewStream(s.ctx, s.sessionID, s.faceSwap),
		release: release,
		done:    make(chan error, 1),
	}
}

// Target은 지금 프레임을 처리하는 워커 주소다.
func (s *SwitchableStream) Target() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.client.Address()
}

func (s *SwitchableStream) Process(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) (*aiv1.ProcessedVideoChunk, error) {
	s.processMu.Lock()
	defer s.processMu.Unlock()

	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return nil, ErrSwitchClosed
	}
	s.lastProcessAt = time.Now()
	if s.candidate != nil && s.candidate.ready {
		s.promoteLocked()
	}
	current, candidate := s.current, s.candidate
	s.mu.Unlock()

	if candidate != nil && candidate.busy.CompareAndSwap(false, true) {
		// 호출자는 반환 뒤 프레임 버퍼를 다시 쓸 수 있으므로 사본을 넘긴다.
		go s.warm(candidate, append([]byte(nil), data...), timestamp, width, height, pixFmt, proto.Clone(mosaic).(*aiv1.MosaicConfig))
	}
	return current.stream.Process(data, timestamp, width, height, pixFmt, mosaic)
}

// warm은 후보 워커에 예열 프레임 하나를 보내고 결과를 반영한다.
func (s *SwitchableStream) warm(candidate *switchLeg, data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) {
	defer candidate.busy.Store(false)
	response, err := candidate.stream.Process(data, timestamp, width, height, pixFmt, mosaic)
	if err == nil {
		err = checkResponse(response, timestamp)
	}
	if err != nil {
		s.dropCandidate(candidate, fmt.Errorf("warm AI target %s: %w", candidate.client.Address(), err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidate != candidate {
		return
	}
	candidate.warmed++
	if candidate.warmed >= s.warmFrames {
		candidate.ready = true
	}
}

// checkResponse는 Processor가 프레임마다 요구하는 응답 계약을 확인한다.
func checkResponse(response *aiv1.ProcessedVideoChunk, timestamp int64) error {
	if response.GetTimestamp() != timestamp {
		return fmt.Errorf("timestamp mismatch: sent=%d received=%d", timestamp, response.GetTimestamp())
	}
	if code := response.GetErrorCode(); code != "" {
		return fmt.Errorf("error_code=%s error_message=%q", code, response.GetErrorMessage())
	}
	if !strings.EqualFold(response.GetStatusMessage(), "success") {
		return fmt.Errorf("status=%q", response.GetStatusMessage())
	}
	return nil
}

// promoteLocked는 예열을 마친 후보를 지금 워커로 올리고 이전 워커를 놓는다. s.mu와
// processMu를 쥔 채 불린다 — 이전 워커 스트림에 처리 중인 프레임이 없다.
func (s *SwitchableStream) promoteLocked() {
	previous := s.current
	s.current, s.candidate = s.candidate, nil
	previous.discard()
	s.current.done <- nil
}

// dropCandidate는 후보가 아직 후보이면 버리고 err로 전환을 끝낸다.
func (s *SwitchableStream) dropCandidate(candidate *switchLeg, err error) {
	s.mu.Lock()
	if s.candidate != candidate {
		s.mu.Unlock()
		return
	}
	s.candidate = nil
	s.mu.Unlock()
	candidate.discard()
	candidate.done <- err
}

// SwitchTo는 세션 스트림을 client로 넘기고 넘어가면 돌아온다. release는 client
// 배정을 되돌리며, 전환이 실패하거나 나중에 client를 떠날 때 한 번 불린다. 예열 중
// ctx가 끝나면 지금 워커를 그대로 두고 ctx 오류를 돌려준다.
func (s *SwitchableStream) SwitchTo(ctx context.Context, client *Client, release func()) error {
	leg := s.newLeg(client, release)

	s.mu.Lock()
	if err := s.switchableLocked(client); err != nil {
		s.mu.Unlock()
		leg.discard()
		return err
	}
	idle := s.lastProcessAt.IsZero() || time.Since(s.lastProcessAt) > idleSwitchAfter
	if !idle {
		s.candidate = leg
		s.mu.Unlock()
		return s.awaitSwitch(ctx, leg)
	}
	s.mu.Unlock()

	// 프레임이 흐르지 않는 스트림은 두 프레임 사이를 잡아 바로 바꾼다.
	s.processMu.Lock()
	defer s.processMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.switchableLocked(client); err != nil {
		leg.discard()
		return err
	}
	s.candidate = leg
	s.promoteLocked()
	<-leg.done
	return nil
}

func (s *SwitchableStream) switchableLocked(client *Client) error {
	switch {
	case s.released:
		return ErrSwitchClosed
	case s.candidate != nil:
		return ErrSwitchInProgress
	case s.current.client == client:
		return fmt.Errorf("AI stream is already on %s", client.Address())
	}
	return nil
}

func (s *SwitchableStream) awaitSwitch(ctx context.Context, leg *switchLeg) error {
	// 예열 중 프레임이 끊기면(일시 정지 등) 예열할 프레임도 없으므로 처음부터 멈춰 있던
	// 스트림처럼 바로 바꾼다.
	idleCheck := time.NewTicker(idleSwitchAfter / 4)
	defer idleCheck.Stop()
wait:
	for {
		select {
		case err := <-leg.done:
			return err
		case <-ctx.Done():
			break wait
		case <-idleCheck.C:
			s.promoteIfIdle(leg)
		}
	}
	s.mu.Lock()
	if s.candidate == leg && !leg.ready {
		s.candidate = nil
		s.mu.Unlock()
		// 예열 프레임이 처리 중일 수 있어 기다리지 않고 닫는다.
		go leg.discard()
		return ctx.Err()
	}
	s.mu.Unlock()
	// 그 사이 넘어갔거나 버려졌다. 어느 쪽이든 결과가 온다.
	return <-leg.done
}

// promoteIfIdle은 후보 leg가 아직 후보이고 idleSwitchAfter 넘게 프레임이 없었으면
// 예열을 마치지 않았어도 올린다. 처리 중인 예열 프레임이 있으면 그 결과를 기다린다.
func (s *SwitchableStream) promoteIfIdle(leg *switchLeg) {
	s.processMu.Lock()
	defer s.processMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidate == leg && !leg.busy.Load() && time.Since(s.lastProcessAt) > idleSwitchAfter {
		s.promoteLocked()
	}
}

// Close는 열린 워커 스트림을 닫는다. 배정은 그대로이며 다음 Process가 스트림을 다시
// 연다(SuspendAIInput). 예열 중이던 전환은 그만둔다.
func (s *SwitchableStream) Close() {
	s.mu.Lock()
	current, candidate := s.current, s.candidate
	s.candidate = nil
	s.mu.Unlock()
	current.stream.Close()
	if candidate != nil {
		// 예열 프레임이 처리 중일 수 있어 기다리지 않고 닫는다.
		go candidate.discard()
		candidate.done <- ErrSwitchInterrupted
	}
}

// Release는 파이프라인이 끝날 때 한 번 불러 스트림을 닫고 워커 배정을 돌려준다.
func (s *SwitchableStream) Release() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	current, candidate := s.current, s.candidate
	s.candidate = nil
	s.mu.Unlock()
	current.discard()
	if candidate != nil {
		go candidate.discard()
		candidate.done <- ErrSwitchClosed
	}
}
