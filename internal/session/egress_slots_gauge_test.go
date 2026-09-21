package session

import (
	"bufio"
	"io"
	"log/slog"
	"strings"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// slotCapacityGauge는 아직 송출이 한 번도 없었던 서버가 내보내는 상한 값이다.
func slotCapacityGauge(t *testing.T, maxEgressSlots int) string {
	t.Helper()
	registry := metrics.New()
	manager, err := NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxEgressSlots: maxEgressSlots,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)

	var rendered strings.Builder
	registry.WritePrometheus(&rendered)
	scanner := bufio.NewScanner(strings.NewReader(rendered.String()))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "innolive_egress_slots_capacity "); ok {
			return value
		}
	}
	t.Fatal("innolive_egress_slots_capacity missing from /metrics")
	return ""
}

// TestSlotCapacityGaugeIsSetBeforeFirstStream: 상한은 첫 송출을 기다리지 않고
// 기동과 함께 나와야 한다. 이 게이지에서 0은 "제한 없음"이라, 설정해 둔 상한이
// 첫 방송 전까지 0으로 보이면 운영자는 설정이 안 먹은 것으로 읽는다.
func TestSlotCapacityGaugeIsSetBeforeFirstStream(t *testing.T) {
	if capacity := slotCapacityGauge(t, 12); capacity != "12" {
		t.Fatalf("capacity = %s, want 12 before any stream starts", capacity)
	}
}

// TestSlotCapacityGaugeStaysZeroWhenUnlimited: MAX_EGRESS_SLOTS 미설정 배포는
// 종전대로 0(제한 없음)이다.
func TestSlotCapacityGaugeStaysZeroWhenUnlimited(t *testing.T) {
	if capacity := slotCapacityGauge(t, 0); capacity != "0" {
		t.Fatalf("capacity = %s, want 0 for an unlimited deployment", capacity)
	}
}
