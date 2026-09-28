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

// Policy는 플랜 하나의 정책 값이다. 시간 값 0은 무제한이다.
type Policy struct {
	MonthlyBroadcast time.Duration
	MaxPerBroadcast  time.Duration
	// AllowedModes가 비어 있으면 서버 송출이 없는 플랜이다(Glow, 온디바이스).
	AllowedModes []Mode
	FaceSlots    int
}

var policies = map[Plan]Policy{
	Spark: {
		MonthlyBroadcast: 5 * time.Hour,
		MaxPerBroadcast:  2 * time.Hour,
		AllowedModes:     []Mode{Mode720pSingle},
		FaceSlots:        2,
	},
	Glow: {
		FaceSlots: 2,
	},
	Beam: {
		MonthlyBroadcast: 120 * time.Hour,
		MaxPerBroadcast:  8 * time.Hour,
		AllowedModes:     []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti},
		FaceSlots:        5,
	},
	Plasma: {
		MonthlyBroadcast: 240 * time.Hour,
		MaxPerBroadcast:  12 * time.Hour,
		AllowedModes:     []Mode{Mode720pSingle, ModeFHDSingle, Mode720pMulti, ModeFHDMulti},
		FaceSlots:        10,
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

// Allows는 플랜이 그 송출 방식을 허용하는지다.
func (p Plan) Allows(mode Mode) bool {
	for _, allowed := range policies[p].AllowedModes {
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
	policy.AllowedModes = append([]Mode(nil), policy.AllowedModes...)
	return policy, true
}
