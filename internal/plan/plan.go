// Package plan은 요금제 플랜과 플랜별 정책 값을 한 곳에 둔다(#270). 정본은
// 노션 InnoLive/BM이며, 값이 바뀌면 여기만 고친다. 다른 internal 패키지에
// 의존하지 않아 auth·session·server 어디서든 읽을 수 있다.
package plan

import "time"

// Plan은 사용자의 요금제다. DB(users.plan)와 API에 이 문자열 그대로 쓴다.
type Plan string

const (
	Spark  Plan = "spark"
	Glow   Plan = "glow"
	Beam   Plan = "beam"
	Plasma Plan = "plasma"

	// Default는 가입 직후와 기존 사용자의 플랜이다.
	Default = Spark
)

// Mode는 송출 방식이다 — 해상도와 동시 송출 여부의 조합.
type Mode string

const (
	Mode720pSingle Mode = "720p_single"
	ModeFHDSingle  Mode = "fhd_single"
	Mode720pMulti  Mode = "720p_multi"
	ModeFHDMulti   Mode = "fhd_multi"
)

// Modes는 송출 방식 전체를 배수 순서로 늘어놓은 것이다. 남은 시간 안내(#276)가
// 플랜이 허용하지 않는 방식까지 잠금으로 보여 주려고 쓴다.
var Modes = []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti, ModeFHDMulti}

// Units는 그 송출 방식의 서버 유닛, 곧 방송 시간 차감 배수다.
func (m Mode) Units() int {
	switch m {
	case ModeFHDSingle:
		return Units(true, 1)
	case Mode720pMulti:
		return Units(false, 2)
	case ModeFHDMulti:
		return Units(true, 2)
	default:
		return Units(false, 1)
	}
}

// Policy는 플랜 하나의 정책 값이다. 시간 값 0은 무제한이다. 처리 위치(서버·
// 온디바이스)별로 월 시간과 송출 방식을 따로 둔다(#468).
type Policy struct {
	// MonthlyServer는 서버 처리 월 한도다. 송출 방식 배수로 차감하는 유닛-시간이다.
	MonthlyServer time.Duration
	// MonthlyDevice는 온디바이스 월 한도다. 실제 방송 시간으로 센다.
	MonthlyDevice time.Duration
	// MaxPerBroadcast는 처리 위치와 무관한 1회 최대(실제 방송 시간)다.
	MaxPerBroadcast time.Duration
	// ServerModes가 비어 있으면 서버 송출이 없는 플랜이다(Glow).
	ServerModes []Mode
	DeviceModes []Mode
	FaceSlots   int
}

// Monthly는 처리 위치의 월 한도다.
func (p Policy) Monthly(onDevice bool) time.Duration {
	if onDevice {
		return p.MonthlyDevice
	}
	return p.MonthlyServer
}

// Modes는 처리 위치에서 허용하는 송출 방식이다.
func (p Policy) Modes(onDevice bool) []Mode {
	if onDevice {
		return p.DeviceModes
	}
	return p.ServerModes
}

// 정본은 노션 신규 BM(2026-10-13)과 2026-10-15 확정(Glow 온디바이스 FHD 한 곳).
// 온디바이스 송출 방식은 상한이며 그보다 낮은 방식을 모두 포함한다.
var policies = map[Plan]Policy{
	Spark: {
		// Spark는 서버를 쓰지 않는다(#470). 웹 게스트 체험은 플랜이 없어 영향받지 않는다.
		MonthlyDevice:   5 * time.Hour,
		MaxPerBroadcast: time.Hour,
		DeviceModes:     []Mode{Mode720pSingle},
		FaceSlots:       2,
	},
	Glow: {
		MonthlyDevice:   60 * time.Hour,
		MaxPerBroadcast: 8 * time.Hour,
		DeviceModes:     []Mode{Mode720pSingle, ModeFHDSingle},
		FaceSlots:       2,
	},
	Beam: {
		MonthlyServer:   40 * time.Hour,
		MonthlyDevice:   120 * time.Hour,
		MaxPerBroadcast: 12 * time.Hour,
		ServerModes:     []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti},
		DeviceModes:     []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti},
		FaceSlots:       5,
	},
	Plasma: {
		MonthlyServer:   80 * time.Hour,
		MonthlyDevice:   300 * time.Hour,
		MaxPerBroadcast: 12 * time.Hour,
		ServerModes:     []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti, ModeFHDMulti},
		DeviceModes:     []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti, ModeFHDMulti},
		FaceSlots:       10,
	},
}

// ModeFor는 해상도와 송출 대상 수로 송출 방식을 정한다. 대상이 둘 이상이면
// 동시 송출이다.
func ModeFor(fhd bool, targets int) Mode {
	switch {
	case fhd && targets > 1:
		return ModeFHDMulti
	case fhd:
		return ModeFHDSingle
	case targets > 1:
		return Mode720pMulti
	default:
		return Mode720pSingle
	}
}

// Units는 송출 대상 targets개를 동시에 내보낼 때의 서버 유닛이다(노션 BM §2).
// 720p 한 곳 1 · FHD 한 곳 2 · 720p 동시 2 · FHD 동시 3 — 720p는 대상 수,
// FHD는 대상 수 + 1이다. 방송 시간 1시간 = 유닛 1개를 1시간 쓴 것이다.
func Units(fhd bool, targets int) int {
	if targets <= 0 {
		return 0
	}
	if fhd {
		return targets + 1
	}
	return targets
}

// Allows는 플랜이 그 처리 위치에서 송출 방식을 허용하는지다.
func (p Plan) Allows(mode Mode, onDevice bool) bool {
	for _, allowed := range policies[p].Modes(onDevice) {
		if allowed == mode {
			return true
		}
	}
	return false
}

// Valid는 알려진 플랜인지 확인한다.
func (p Plan) Valid() bool {
	_, ok := policies[p]
	return ok
}

// Policy는 플랜의 정책 값을 돌려준다. 알 수 없는 플랜이면 false다.
func (p Plan) Policy() (Policy, bool) {
	policy, ok := policies[p]
	if !ok {
		return Policy{}, false
	}
	policy.ServerModes = append([]Mode(nil), policy.ServerModes...)
	policy.DeviceModes = append([]Mode(nil), policy.DeviceModes...)
	return policy, true
}
