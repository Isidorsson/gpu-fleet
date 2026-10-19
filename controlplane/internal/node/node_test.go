package node

import (
	"errors"
	"testing"
)

func mustApply(t *testing.T, n Node, ev Event, tenant string) Node {
	t.Helper()
	next, err := n.Apply(ev, tenant)
	if err != nil {
		t.Fatalf("Apply(%s): %v", ev, err)
	}
	return next
}

func TestRegisterStartsRacked(t *testing.T) {
	n := Register("gpu-001", "r1", "")
	if n.State != StateRacked || n.Tenant != "" {
		t.Fatalf("Register = %+v, want racked with no tenant", n)
	}
}

func TestTenantLifecycle(t *testing.T) {
	n := Register("gpu-001", "r1", "")
	n = mustApply(t, n, EventStartInspection, "")
	n = mustApply(t, n, EventInspectionPassed, "")
	n = mustApply(t, n, EventProvision, "acme")
	if n.Tenant != "acme" {
		t.Fatalf("after provision tenant = %q, want acme", n.Tenant)
	}
	n = mustApply(t, n, EventProvisionSucceeded, "")
	n = mustApply(t, n, EventRelease, "")
	if n.Tenant != "acme" {
		t.Fatalf("tenant cleared before wipe: %+v", n)
	}
	n = mustApply(t, n, EventWipeSucceeded, "")
	if n.Tenant != "" || n.State != StateInspecting {
		t.Fatalf("after wipe = %+v, want inspecting with no tenant", n)
	}
}

func TestQuarantineKeepsTenantUntilWiped(t *testing.T) {
	n := Node{ID: "gpu-001", State: StateActive, Tenant: "acme"}
	n = mustApply(t, n, EventHardwareFault, "")
	n = mustApply(t, n, EventRepaired, "")
	if n.Tenant != "acme" || n.State != StateWiping {
		t.Fatalf("after repair = %+v, want wiping still owned by acme", n)
	}
}

func TestApplyTenantRules(t *testing.T) {
	available := Node{ID: "gpu-001", State: StateAvailable}
	if _, err := available.Apply(EventProvision, ""); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("provision without tenant: err = %v", err)
	}
	if _, err := available.Apply(EventHardwareFault, "acme"); !errors.Is(err, ErrUnexpectedTenant) {
		t.Errorf("non-provision with tenant: err = %v", err)
	}
}

func TestApplyInvalidTransitionLeavesNodeUnchanged(t *testing.T) {
	n := Register("gpu-001", "r1", "")
	got, err := n.Apply(EventRelease, "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if got != n {
		t.Fatalf("node changed on error: %+v", got)
	}
}

func TestParseEvent(t *testing.T) {
	for _, ev := range allEvents {
		if got, err := ParseEvent(string(ev)); err != nil || got != ev {
			t.Errorf("ParseEvent(%q) = %q, %v", ev, got, err)
		}
	}
	if _, err := ParseEvent("explode"); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("ParseEvent(explode): err = %v", err)
	}
}

func TestParseState(t *testing.T) {
	for _, st := range allStates {
		if got, err := ParseState(string(st)); err != nil || got != st {
			t.Errorf("ParseState(%q) = %q, %v", st, got, err)
		}
	}
	if _, err := ParseState("on_fire"); !errors.Is(err, ErrUnknownState) {
		t.Errorf("ParseState(on_fire): err = %v", err)
	}
}

// Keeps allStates and allEvents in sync with the transition table.
func TestEnumListsCoverTransitionTable(t *testing.T) {
	for from, events := range transitions {
		if _, err := ParseState(string(from)); err != nil {
			t.Errorf("state %q missing from allStates", from)
		}
		for ev, to := range events {
			if _, err := ParseEvent(string(ev)); err != nil {
				t.Errorf("event %q missing from allEvents", ev)
			}
			if _, err := ParseState(string(to)); err != nil {
				t.Errorf("state %q missing from allStates", to)
			}
		}
	}
}
