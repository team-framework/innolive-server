package session

import (
	"errors"
	"time"

	"inno-live-server/internal/media"

	"github.com/pion/rtcp"
)

// 송출 해상도는 세션 생성 시 정해진다(#271). 디코더가 트랙 도착 즉시 이
// 해상도로 핀을 걸고 뜨므로, 방송 준비(prepare) 시점에는 이미 늦다. 세션 중
// 변경(#283)은 모든 송출이 멈춘 동안에만 받고, 파이프라인을 새 핀으로 다시 띄운다.
const (
	Resolution720p = "720p"
	ResolutionFHD  = "fhd"

	// fhdPinLongEdge는 FHD 세션의 디코더 장변이다. AI 서버 MAX_LONG_EDGE와 같다.
	fhdPinLongEdge = 1920
)

var ErrInvalidResolution = errors.New("broadcast_resolution must be 720p or fhd")

// ErrResolutionChangeWhileLive는 멈추지 않은 송출이 있어 해상도를 바꿀 수 없는
// 경우다. 해상도는 세션에 하나라, 한 대상만 멈춘 채 바꾸면 방송 중인 대상의
// 화질이 바뀐다.
var ErrResolutionChangeWhileLive = errors.New("every stream must be paused to change the resolution")

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

// ChangeBroadcastResolution은 세션의 송출 해상도를 바꾼다(#283). 송출 중인 대상이
// 있으면 전부 멈춰 있어야 한다. 유닛을 먼저 다시 잡고(자리가 없으면 기존 해상도
// 유지), 영상 파이프라인을 새 디코더 핀으로 다시 띄운다. egress는 재개 뒤 들어오는
// 새 치수의 프레임을 보고 같은 스트림 키로 ffmpeg만 다시 띄운다.
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
	if hasUnpausedTargetLocked(s) {
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

// hasUnpausedTargetLocked는 멈추지 않은 채 송출 중인 대상이 있는지다. Session.mu를
// 가진 호출자만 쓴다.
func hasUnpausedTargetLocked(s *Session) bool {
	phases := make([]media.EgressPhase, 0, len(s.targets))
	for _, t := range s.targets {
		if t.egress == nil || t.stopReason != nil {
			continue
		}
		phases = append(phases, t.egress.Status().Phase)
	}
	return anyTargetUnpaused(phases)
}

// anyTargetUnpaused는 대상 단계 중 멈추지도 끝나지도 않은 것이 있는지다. 송출 전
// (대상 없음)이면 false다 — 해상도를 바꿔도 방송 화질이 바뀌지 않는다.
func anyTargetUnpaused(phases []media.EgressPhase) bool {
	for _, phase := range phases {
		switch phase {
		case media.EgressPhaseStopped, media.EgressPhasePaused, media.EgressPhasePausedReconfiguring, media.EgressPhasePausedReconnecting:
			continue
		default:
			return true
		}
	}
	return false
}
