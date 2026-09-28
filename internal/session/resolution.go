package session

import (
	"context"
	"errors"
	"time"

	"inno-live-server/internal/media"

	"github.com/pion/rtcp"
)

// 송출 해상도는 세션 생성 시 정해진다(#271). 디코더가 트랙 도착 즉시 이
// 해상도로 핀을 걸고 뜨므로, 방송 준비(prepare) 시점에는 이미 늦다. 세션 중
// 변경(#283)은 송출 대상이 하나도 없을 때만 받고, 파이프라인을 새 핀으로 다시
// 띄운다. 유튜브·치지직 모두 같은 방송 안의 해상도 변경을 반영하지 않아
// (2026-09-28 실측) 방송 중 변경은 서버가 방송을 끝내고 새로 여는 것으로 한다.
const (
	Resolution720p = "720p"
	ResolutionFHD  = "fhd"

	// fhdPinLongEdge는 FHD 세션의 디코더 장변이다. AI 서버 MAX_LONG_EDGE와 같다.
	fhdPinLongEdge = 1920
)

var ErrInvalidResolution = errors.New("broadcast_resolution must be 720p or fhd")

// ErrResolutionChangeWhileLive는 준비·송출 중인 대상이 있어 해상도를 바꿀 수 없는
// 경우다. 플랫폼은 같은 방송 안의 해상도 변경을 반영하지 않는다.
var ErrResolutionChangeWhileLive = errors.New("stop every broadcast before changing the resolution")

// normalizeResolution은 빈 값을 720p로 채우고 미지원 값을 거절한다 — 해상도를
// 보내지 않는 기존 클라이언트는 종전대로 720p다.
func normalizeResolution(value string) (string, error) {
	switch value {
	case "", Resolution720p:
		return Resolution720p, nil
	case ResolutionFHD:
		return ResolutionFHD, nil
	default:
		return "", ErrInvalidResolution
	}
}

// pinLongEdgeFor는 세션의 디코더 장변을 정한다. 전역 핀이 꺼져 있으면(0,
// 로컬·벤치) 종전대로 핀을 걸지 않는다. 켜져 있으면 720p는 그 값(프로덕션
// 1280), FHD는 1920이다.
func pinLongEdgeFor(resolution string, globalPin int) int {
	if globalPin == 0 {
		return 0
	}
	if resolution == ResolutionFHD {
		return fhdPinLongEdge
	}
	return globalPin
}

// Resolution은 현재 송출 해상도다.
func (s *Session) Resolution() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BroadcastResolution
}

// ChangeBroadcastResolution은 세션의 송출 해상도를 바꾸고(#283) 영상 파이프라인을
// 새 디코더 핀으로 다시 띄운다. 준비·송출 중인 대상이 있으면 거절한다 — 방송 중
// 변경은 서버가 대상을 먼저 끝낸 뒤 이 함수를 부른다.
func (m *Manager) ChangeBroadcastResolution(id, value string) (*Session, error) {
	resolution, err := normalizeResolution(value)
	if err != nil {
		return nil, err
	}
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.resolutionMu.Lock()
	defer s.resolutionMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if s.BroadcastResolution == resolution {
		s.mu.Unlock()
		return s, nil
	}
	if hasActiveTargetLocked(s) {
		s.mu.Unlock()
		return nil, ErrResolutionChangeWhileLive
	}
	if err := m.egressSlots.ChangeResolution(s.ID, resolution == ResolutionFHD); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	previous := s.BroadcastResolution
	s.BroadcastResolution = resolution
	s.pinLongEdge = pinLongEdgeFor(resolution, m.cfg.DecoderPinLongEdge)
	track, done := s.videoTrack, s.pipelineDone
	// 핀이 같으면(전역 핀이 꺼진 로컬·벤치) 디코더 출력이 그대로라 다시 띄울 것이 없다.
	restart := s.pinLongEdge != pinLongEdgeFor(previous, m.cfg.DecoderPinLongEdge) &&
		track != nil && s.rawTrackID == track.ID() && s.trackCancel != nil
	if restart {
		s.trackCancel()
		s.trackCancel = nil
	}
	s.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()

	m.recordUsage(UsageEvent{Kind: UsageResolutionChanged, SessionID: s.ID, Resolution: resolution})
	m.publishEgressSlots()
	m.logger.Info("broadcast resolution changed", "session_id", s.ID, "from", previous, "to", resolution, "restart_pipeline", restart)
	if !restart {
		return s, nil
	}
	// 이전 파이프라인이 트랙 읽기를 놓을 때까지 기다린다 — 두 파이프라인이 한
	// 트랙을 나눠 읽으면 안 된다.
	<-done
	s.mu.RLock()
	// 기다리는 사이 세션이 닫혔거나 트랙이 교체됐으면 그쪽이 새 핀으로 이미 띄웠다.
	current := !s.closed && s.videoTrack == track && s.trackCancel == nil
	s.mu.RUnlock()
	if !current {
		return s, nil
	}
	m.startVideoPipeline(s.baseCtx, s, track, media.VideoCodec(track.Codec().MimeType))
	// 새 디코더는 참조 프레임이 없으므로 송출자에게 키프레임을 청한다.
	if err := s.PC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil {
		m.logger.Warn("keyframe request failed", "session_id", s.ID, "error", err)
	}
	return s, nil
}

// hasActiveTargetLocked는 준비·송출 중이거나 egress가 아직 살아 있는 대상이
// 있는지다. 중지가 요청된 egress는 끝나는 중이므로 세지 않는다. Session.mu를
// 가진 호출자만 쓴다.
func hasActiveTargetLocked(s *Session) bool {
	for _, t := range s.targets {
		if t.phase != BroadcastPhaseIdle {
			return true
		}
		if t.egress != nil && t.stopReason == nil && t.egress.Status().Phase != media.EgressPhaseStopped {
			return true
		}
	}
	return false
}

// WaitStreamEnded는 대상의 마지막 egress 세대가 끝날 때까지 기다린다. 중지는
// egress 종료를 요청만 하고, 끝난 egress의 정리(단계·방송 정리)는 그 뒤에 온다 —
// 곧바로 같은 대상을 다시 준비하면 늦게 온 정리가 새 준비를 지운다.
func (m *Manager) WaitStreamEnded(ctx context.Context, id, provider string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.mu.RLock()
	done := s.readTarget(provider).done
	s.mu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CheckBroadcastUnits는 방송 중 송출 방식 전환(#283·#300) 전에, 이 세션이 resolution
// 해상도로 count개를 송출할 자리가 되는지 본다(잡지는 않는다). 자리가 없으면 방송을
// 끊기 전에 실패한다. 지금 쥔 유닛은 전환 중 반납되므로 빼고 센다.
func (m *Manager) CheckBroadcastUnits(id, value string, count int) error {
	resolution, err := normalizeResolution(value)
	if err != nil {
		return err
	}
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	claim := egressClaimFor(s)
	claim.HighRes = resolution == ResolutionFHD
	return m.egressSlots.CheckClaim(claim, count)
}
