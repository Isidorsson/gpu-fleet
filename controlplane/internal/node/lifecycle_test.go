package node

import (
	"errors"
	"testing"
)

func TestNextValidTransitions(t *testing.T) {
	tests := []struct {
		from State
		ev   Event
		want State
	}{
		{StateRacked, EventStartInspection, StateInspecting},
		{StateInspecting, EventInspectionPassed, StateAvailable},
		{StateInspecting, EventInspectionFailed, StateQuarantined},
		{StateAvailable, EventProvision, StateProvisioning},
		{StateProvisioning, EventProvisionSucceeded, StateActive},
		{StateProvisioning, EventProvisionFailed, StateWiping},
		{StateActive, EventRelease, StateWiping},
		{StateWiping, EventWipeSucceeded, StateInspecting},
		{StateWiping, EventWipeFailed, StateQuarantined},
		{StateQuarantined, EventRepaired, StateWiping},
	}
	for _, tt := range tests {
		got, err := Next(tt.from, tt.ev)
		if err != nil {
			t.Errorf("Next(%s, %s): unexpected error: %v", tt.from, tt.ev, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Next(%s, %s) = %s, want %s", tt.from, tt.ev, got, tt.want)
		}
	}
}

func TestNextRejectsInvalidTransitions(t *testing.T) {
	tests := []struct {
		from State
		ev   Event
	}{
		{StateRacked, EventProvision},
		{StateAvailable, EventRelease},
		{StateActive, EventProvision},
		{StateWiping, EventInspectionPassed},
		{StateQuarantined, EventHardwareFault},
		{State("bogus"), EventStartInspection},
	}
	for _, tt := range tests {
		got, err := Next(tt.from, tt.ev)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("Next(%s, %s): err = %v, want ErrInvalidTransition", tt.from, tt.ev, err)
		}
		if got != tt.from {
			t.Errorf("Next(%s, %s) = %s, want unchanged state", tt.from, tt.ev, got)
		}
	}
}

func TestHardwareFaultQuarantinesPoweredStates(t *testing.T) {
	for _, s := range []State{StateInspecting, StateAvailable, StateProvisioning, StateActive, StateWiping} {
		got, err := Next(s, EventHardwareFault)
		if err != nil || got != StateQuarantined {
			t.Errorf("Next(%s, hardware_fault) = %s, %v; want quarantined", s, got, err)
		}
	}
}

// Walks the whole graph and checks the core safety invariant: available is
// only ever entered from inspecting.
func TestAvailableOnlyReachableFromInspecting(t *testing.T) {
	for from, events := range transitions {
		for ev, to := range events {
			if to == StateAvailable && from != StateInspecting {
				t.Errorf("%s --%s--> available bypasses inspection", from, ev)
			}
		}
	}
}

func TestTenantLeavesOnlyThroughWiping(t *testing.T) {
	tenantStates := map[State]bool{StateProvisioning: true, StateActive: true}
	for from, events := range transitions {
		if !tenantStates[from] {
			continue
		}
		for ev, to := range events {
			if !tenantStates[to] && to != StateWiping && to != StateQuarantined {
				t.Errorf("%s --%s--> %s releases a tenant node without wiping", from, ev, to)
			}
		}
	}
}
