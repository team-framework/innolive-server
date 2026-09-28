package session

import (
	"testing"

	"inno-live-server/internal/plan"
)

func TestEgressClaimFor(t *testing.T) {
	cases := []struct {
		plan       plan.Plan
		resolution string
		highRes    bool
		capped     bool
		group      string
	}{
		{plan.Spark, Resolution720p, false, true, "spark"},
		{plan.Plasma, ResolutionFHD, true, false, "plasma"},
		{plan.Beam, Resolution720p, false, false, "beam"},
		// 인증을 끈 벤치처럼 플랜이 없는 세션은 상한 없이 센다.
		{"", Resolution720p, false, false, "none"},
	}
	for _, test := range cases {
		claim := egressClaimFor(&Session{ID: "s-1", Plan: test.plan, BroadcastResolution: test.resolution})
		if claim.Owner != "s-1" || claim.HighRes != test.highRes || claim.Capped != test.capped || claim.Group != test.group {
			t.Fatalf("%q/%s: claim = %+v", test.plan, test.resolution, claim)
		}
	}
}
