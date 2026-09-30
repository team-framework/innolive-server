package media

import (
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// newLossyAssembler는 재정렬 창이 작아 시퀀스가 건너뛰면 샘플 빌더가 구간을
// 포기하고 PrevDroppedPackets를 알리는 조립기를 만든다.
func newLossyAssembler(t *testing.T, minInterval time.Duration, requestKeyframe func()) *rtpFrameAssembler {
	t.Helper()
	assembler, err := newRTPFrameAssemblerWithLimits(
		metrics.New(),
		config.PrivacyModeBypass,
		VideoCodecVP8,
		4,
		10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler.requestKeyframe = requestKeyframe
	assembler.keyframeMinInterval = minInterval
	return assembler
}

// pushLoss는 미완성 프레임 뒤에 한참 앞선 완성 프레임을 넣어, 샘플 빌더가 부분
// 프레임의 패킷을 버리게 한다.
func pushLoss(assembler *rtpFrameAssembler, base uint16, timestamp uint32) {
	assembler.push(vp8Packet(base, timestamp, false, true, "partial"))
	for offset := uint16(1); offset <= 8; offset++ {
		assembler.push(vp8Packet(base+100+offset, timestamp+uint32(offset)*3000, true, true, "whole"))
	}
}

// TestAssemblerRequestsKeyframeAfterDroppedPackets는 #93의 회복 경로를 검증한다.
// 버린 RTP 패킷은 디코더가 방금 참조를 잃었다는 뜻이고, 송신자가 새 키프레임을
// 보낼 때까지 손상이 모든 출력 프레임(키프레임 포함)에 다시 인코딩된다. 키프레임을
// 요청하지 않고 손실만 세면 송신자 자체 키프레임 주기(Chrome 실측 약 20초) 동안
// 화면이 깨져 있다.
func TestAssemblerRequestsKeyframeAfterDroppedPackets(t *testing.T) {
	var requests int
	assembler := newLossyAssembler(t, 0, func() { requests++ })

	pushLoss(assembler, 100, 3000)

	if requests == 0 {
		t.Fatalf("assembler discarded packets without requesting a keyframe")
	}
}

// TestAssemblerThrottlesKeyframeRequests는 송신자에게 요청이 쏟아지지 않게 한다. 잃은
// 구간 하나가 여러 샘플에 걸쳐 드러나고, 송신자가 키프레임을 인코딩해 보낼 시간이
// 있어야 다음 요청이 쓸모 있다.
func TestAssemblerThrottlesKeyframeRequests(t *testing.T) {
	var requests int
	assembler := newLossyAssembler(t, time.Hour, func() { requests++ })

	pushLoss(assembler, 100, 3000)
	pushLoss(assembler, 500, 90000)

	if requests != 1 {
		t.Fatalf("keyframe requests = %d, want 1 within the throttle window", requests)
	}
}

// TestAssemblerWithoutKeyframeRequesterDoesNotPanic: 송신자로 가는 피드백 채널이 없는
// 호출자도 조립기를 쓸 수 있다.
func TestAssemblerWithoutKeyframeRequesterDoesNotPanic(t *testing.T) {
	assembler := newLossyAssembler(t, 0, nil)
	pushLoss(assembler, 100, 3000)
}

// TestAssemblerDoesNotRequestKeyframeWithoutLoss: 깨끗한 스트림은 송신자에게 아무것도
// 요청하지 않는다.
func TestAssemblerDoesNotRequestKeyframeWithoutLoss(t *testing.T) {
	var requests int
	assembler := newLossyAssembler(t, 0, func() { requests++ })

	for offset := uint16(0); offset < 8; offset++ {
		assembler.push(vp8Packet(100+offset, 3000+uint32(offset)*3000, true, true, "whole"))
	}

	if requests != 0 {
		t.Fatalf("keyframe requests = %d on a lossless stream, want 0", requests)
	}
}
