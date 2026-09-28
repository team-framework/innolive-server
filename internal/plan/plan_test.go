package plan

import (
	"testing"
	"time"
)

func TestPolicyMatchesBM(t *testing.T) {
	tests := []struct {
		plan    Plan
		monthly time.Duration
		perOnce time.Duration
		modes   int
		faces   int
	}{
		{Spark, 5 * time.Hour, 2 * time.Hour, 1, 2},
		{Glow, 0, 0, 0, 2},
		{Beam, 120 * time.Hour, 8 * time.Hour, 3, 5},
		{Plasma, 240 * time.Hour, 12 * time.Hour, 4, 10},
	}
	for _, test := range tests {
		policy, ok := test.plan.Policy()
		if !ok {
			t.Fatalf("%s: policy missing", test.plan)
		}
		if policy.MonthlyBroadcast != test.monthly || policy.MaxPerBroadcast != test.perOnce ||
			len(policy.AllowedModes) != test.modes || policy.FaceSlots != test.faces {
			t.Fatalf("%s: policy = %+v", test.plan, policy)
		}
	}
}

func TestUnknownPlanIsInvalid(t *testing.T) {
	for _, value := range []Plan{"", "free", "SPARK"} {
		if value.Valid() {
			t.Fatalf("%q must be invalid", value)
		}
		if _, ok := value.Policy(); ok {
			t.Fatalf("%q must have no policy", value)
		}
	}
	if !Default.Valid() {
		t.Fatal("default plan must be valid")
	}
}

func TestPolicyReturnsCopyOfModes(t *testing.T) {
	policy, _ := Plasma.Policy()
	policy.AllowedModes[0] = "tampered"
	again, _ := Plasma.Policy()
	if again.AllowedModes[0] != Mode720pSingle {
		t.Fatal("Policy must not expose the shared slice")
	}
}

func TestModeForAndAllows(t *testing.T) {
	if ModeFor(false, 1) != Mode720pSingle || ModeFor(true, 1) != ModeFHDSingle ||
		ModeFor(false, 2) != Mode720pMulti || ModeFor(true, 2) != ModeFHDMulti {
		t.Fatal("ModeFor mapping is wrong")
	}
	cases := []struct {
		plan Plan
		mode Mode
		want bool
	}{
		{Spark, Mode720pSingle, true}, {Spark, ModeFHDSingle, false}, {Spark, Mode720pMulti, false},
		{Beam, ModeFHDSingle, true}, {Beam, Mode720pMulti, true}, {Beam, ModeFHDMulti, false},
		{Plasma, ModeFHDMulti, true}, {Glow, Mode720pSingle, false}, {"", Mode720pSingle, false},
	}
	for _, test := range cases {
		if got := test.plan.Allows(test.mode); got != test.want {
			t.Fatalf("%q.Allows(%s) = %v, want %v", test.plan, test.mode, got, test.want)
		}
	}
}
