package plan

import (
	"testing"
	"time"
)

func TestPolicyMatchesBM(t *testing.T) {
	tests := []struct {
		plan        Plan
		server      time.Duration
		device      time.Duration
		perOnce     time.Duration
		serverModes int
		deviceModes int
		faces       int
	}{
		{Spark, 0, 5 * time.Hour, time.Hour, 0, 1, 2},
		{Glow, 0, 60 * time.Hour, 8 * time.Hour, 0, 2, 2},
		{Beam, 40 * time.Hour, 120 * time.Hour, 8 * time.Hour, 3, 3, 5},
		{Plasma, 80 * time.Hour, 300 * time.Hour, 12 * time.Hour, 4, 4, 10},
	}
	for _, test := range tests {
		policy, ok := test.plan.Policy()
		if !ok {
			t.Fatalf("%s: policy missing", test.plan)
		}
		if policy.MonthlyServer != test.server || policy.MonthlyDevice != test.device || policy.MaxPerBroadcast != test.perOnce ||
			len(policy.ServerModes) != test.serverModes || len(policy.DeviceModes) != test.deviceModes || policy.FaceSlots != test.faces {
			t.Fatalf("%s: policy = %+v", test.plan, policy)
		}
		if policy.Monthly(false) != test.server || policy.Monthly(true) != test.device {
			t.Fatalf("%s: Monthly mismatch", test.plan)
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
	policy.ServerModes[0] = "tampered"
	policy.DeviceModes[0] = "tampered"
	again, _ := Plasma.Policy()
	if again.ServerModes[0] != Mode720pSingle || again.DeviceModes[0] != Mode720pSingle {
		t.Fatal("Policy must not expose the shared slice")
	}
}

func TestModeForAndAllows(t *testing.T) {
	if ModeFor(false, 1) != Mode720pSingle || ModeFor(true, 1) != ModeFHDSingle ||
		ModeFor(false, 2) != Mode720pMulti || ModeFor(true, 2) != ModeFHDMulti {
		t.Fatal("ModeFor mapping is wrong")
	}
	cases := []struct {
		plan     Plan
		mode     Mode
		onDevice bool
		want     bool
	}{
		{Spark, Mode720pSingle, false, false}, {Spark, ModeFHDSingle, false, false}, {Spark, Mode720pSingle, true, true}, {Spark, ModeFHDSingle, true, false},
		{Glow, Mode720pSingle, false, false}, {Glow, ModeFHDSingle, true, true}, {Glow, Mode720pSingle, true, true}, {Glow, Mode720pMulti, true, false},
		{Beam, ModeFHDSingle, false, true}, {Beam, Mode720pMulti, true, true}, {Beam, ModeFHDMulti, false, false}, {Beam, ModeFHDMulti, true, false},
		{Plasma, ModeFHDMulti, false, true}, {Plasma, ModeFHDMulti, true, true}, {"", Mode720pSingle, true, false},
	}
	for _, test := range cases {
		if got := test.plan.Allows(test.mode, test.onDevice); got != test.want {
			t.Fatalf("%q.Allows(%s, onDevice=%v) = %v, want %v", test.plan, test.mode, test.onDevice, got, test.want)
		}
	}
}

func TestUnitsMatchBM(t *testing.T) {
	cases := []struct {
		fhd     bool
		targets int
		want    int
	}{
		{false, 1, 1}, {true, 1, 2}, {false, 2, 2}, {true, 2, 3}, {false, 0, 0}, {true, 0, 0},
	}
	for _, test := range cases {
		if got := Units(test.fhd, test.targets); got != test.want {
			t.Fatalf("Units(%v, %d) = %d, want %d", test.fhd, test.targets, got, test.want)
		}
	}
}

func TestModeUnitsFollowModeFor(t *testing.T) {
	want := map[Mode]int{Mode720pSingle: 1, ModeFHDSingle: 2, Mode720pMulti: 2, ModeFHDMulti: 3}
	if len(Modes) != len(want) {
		t.Fatalf("Modes = %v", Modes)
	}
	for _, mode := range Modes {
		if mode.Units() != want[mode] {
			t.Fatalf("%s.Units() = %d, want %d", mode, mode.Units(), want[mode])
		}
	}
	// ModeFor와 같은 배수여야 한다 — 차감과 안내가 어긋나면 안 된다.
	for _, c := range []struct {
		fhd     bool
		targets int
	}{{false, 1}, {true, 1}, {false, 2}, {true, 2}} {
		if got := ModeFor(c.fhd, c.targets).Units(); got != Units(c.fhd, c.targets) {
			t.Fatalf("ModeFor(%v,%d).Units() = %d, want %d", c.fhd, c.targets, got, Units(c.fhd, c.targets))
		}
	}
}
