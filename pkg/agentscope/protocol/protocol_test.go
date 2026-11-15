package protocol

import "testing"

func TestLoopStateString(t *testing.T) {
	cases := []struct {
		state LoopState
		want  string
	}{
		{StateReason, "reason"},
		{StateInspect, "inspect"},
		{StateAct, "act"},
		{StateWait, "wait"},
		{StateExit, "exit"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("LoopState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestApprovalPolicyValues(t *testing.T) {
	policies := []ApprovalPolicy{
		ApprovalDefault,
		ApprovalUnlessSafe,
		ApprovalNever,
		ApprovalAlways,
	}
	seen := make(map[ApprovalPolicy]bool)
	for _, p := range policies {
		if seen[p] {
			t.Errorf("duplicate ApprovalPolicy value: %q", p)
		}
		seen[p] = true
		if p == "" {
			t.Error("ApprovalPolicy has empty string value")
		}
	}
}

func TestPermissionProfileValues(t *testing.T) {
	profiles := []PermissionProfile{
		PermDefault,
		PermAcceptEdits,
		PermExplore,
		PermBypass,
		PermDontAsk,
	}
	seen := make(map[PermissionProfile]bool)
	for _, p := range profiles {
		if seen[p] {
			t.Errorf("duplicate PermissionProfile value: %q", p)
		}
		seen[p] = true
		if p == "" {
			t.Error("PermissionProfile has empty string value")
		}
	}
}
