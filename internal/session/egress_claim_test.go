package session

import (
	"testing"

	"inno-live-server/internal/media"
	"inno-live-server/internal/plan"
)

func TestEgressClaimFor(t *testing.T) {
	cases := []struct {
		plan       plan.Plan
		resolution string
		highRes    bool
		tier       int
		group      string
	}{
		{plan.Spark, Resolution720p, false, 0, "spark"},
		{plan.Beam, Resolution720p, false, 1, "beam"},
		{plan.Plasma, ResolutionFHD, true, 2, "plasma"},
		// 인증을 끈 벤치처럼 플랜이 없는 세션은 등급 규칙 없이 센다.
		{"", Resolution720p, false, media.EgressTierUnrestricted, "none"},
	}
	for _, test := range cases {
		claim := egressClaimFor(&Session{ID: "s-1", Plan: test.plan, BroadcastResolution: test.resolution})
		if claim.Owner != "s-1" || claim.HighRes != test.highRes || claim.Tier != test.tier || claim.Group != test.group {
			t.Fatalf("%q/%s: claim = %+v", test.plan, test.resolution, claim)
		}
	}
}
