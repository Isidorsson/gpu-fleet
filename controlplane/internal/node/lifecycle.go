// Package node models the lifecycle of a bare-metal GPU node.
package node

import (
	"errors"
	"fmt"
)

type State string

const (
	StateRacked       State = "racked"
	StateInspecting   State = "inspecting"
	StateAvailable    State = "available"
	StateProvisioning State = "provisioning"
	StateActive       State = "active"
	StateWiping       State = "wiping"
	StateQuarantined  State = "quarantined"
)

type Event string

const (
	EventStartInspection    Event = "start_inspection"
	EventInspectionPassed   Event = "inspection_passed"
	EventInspectionFailed   Event = "inspection_failed"
	EventAttestationFailed  Event = "attestation_failed"
	EventProvision          Event = "provision"
	EventProvisionSucceeded Event = "provision_succeeded"
	EventProvisionFailed    Event = "provision_failed"
	EventRelease            Event = "release"
	EventWipeSucceeded      Event = "wipe_succeeded"
	EventWipeFailed         Event = "wipe_failed"
	EventHardwareFault      Event = "hardware_fault"
	EventRepaired           Event = "repaired"
)

var ErrInvalidTransition = errors.New("invalid transition")

// Every path back to StateAvailable goes through StateInspecting, and every
// path out of a tenant's hands goes through StateWiping, so no node is ever
// handed to a new tenant without a wipe and a fresh acceptance check.
var transitions = map[State]map[Event]State{
	StateRacked: {
		EventStartInspection: StateInspecting,
	},
	StateInspecting: {
		EventInspectionPassed:  StateAvailable,
		EventInspectionFailed:  StateQuarantined,
		EventAttestationFailed: StateQuarantined,
		EventHardwareFault:     StateQuarantined,
	},
	StateAvailable: {
		EventProvision:     StateProvisioning,
		EventHardwareFault: StateQuarantined,
	},
	StateProvisioning: {
		EventProvisionSucceeded: StateActive,
		EventProvisionFailed:    StateWiping,
		EventHardwareFault:      StateQuarantined,
	},
	StateActive: {
		EventRelease:       StateWiping,
		EventHardwareFault: StateQuarantined,
	},
	StateWiping: {
		EventWipeSucceeded: StateInspecting,
		EventWipeFailed:    StateQuarantined,
		EventHardwareFault: StateQuarantined,
	},
	StateQuarantined: {
		EventRepaired: StateWiping,
	},
}

// Next returns the state a node moves to when ev happens in from.
func Next(from State, ev Event) (State, error) {
	to, ok := transitions[from][ev]
	if !ok {
		return from, fmt.Errorf("%w: %q in state %q", ErrInvalidTransition, ev, from)
	}
	return to, nil
}
