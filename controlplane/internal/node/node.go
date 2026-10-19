package node

import (
	"errors"
	"fmt"
	"slices"
)

// Node is a bare-metal machine. Tenant is set while that tenant's data may
// still be on the node's disks, so it survives quarantine and is only cleared
// by a successful wipe. BMC is the base URL of the node's BMC; see
// docs/bmc-protocol.md. Pod is the InfiniBand scalable unit the node's rack
// belongs to; see docs/reservations.md. Reservation is the reservation the
// node was provisioned for, if any; it is kept and cleared like Tenant.
type Node struct {
	ID          string
	Pod         string
	Rack        string
	BMC         string
	State       State
	Tenant      string
	Reservation string
}

// Spec describes a node being registered.
type Spec struct {
	ID   string
	Pod  string
	Rack string
	BMC  string
}

var (
	ErrTenantRequired   = errors.New("tenant required to provision")
	ErrUnexpectedTenant = errors.New("tenant only allowed when provisioning")
	ErrUnknownEvent     = errors.New("unknown event")
	ErrUnknownState     = errors.New("unknown state")
)

var allStates = []State{
	StateRacked, StateInspecting, StateAvailable, StateProvisioning,
	StateActive, StateWiping, StateQuarantined,
}

var allEvents = []Event{
	EventStartInspection, EventInspectionPassed, EventInspectionFailed,
	EventProvision, EventProvisionSucceeded, EventProvisionFailed,
	EventRelease, EventWipeSucceeded, EventWipeFailed,
	EventHardwareFault, EventRepaired,
}

// States returns every lifecycle state.
func States() []State { return slices.Clone(allStates) }

// Register returns a freshly racked node.
func Register(s Spec) Node {
	return Node{ID: s.ID, Pod: s.Pod, Rack: s.Rack, BMC: s.BMC, State: StateRacked}
}

// Apply returns the node after ev. tenant must be set for EventProvision
// and empty for every other event.
func (n Node) Apply(ev Event, tenant string) (Node, error) {
	if ev == EventProvision && tenant == "" {
		return n, ErrTenantRequired
	}
	if ev != EventProvision && tenant != "" {
		return n, ErrUnexpectedTenant
	}
	to, err := Next(n.State, ev)
	if err != nil {
		return n, err
	}

	next := n
	next.State = to
	switch ev {
	case EventProvision:
		next.Tenant, next.Reservation = tenant, ""
	case EventWipeSucceeded:
		next.Tenant, next.Reservation = "", ""
	}
	return next, nil
}

// ProvisionFor provisions n for tenant on behalf of a reservation, which then
// holds the node until a successful wipe.
func (n Node) ProvisionFor(tenant, reservation string) (Node, error) {
	next, err := n.Apply(EventProvision, tenant)
	if err != nil {
		return n, err
	}
	next.Reservation = reservation
	return next, nil
}

func ParseEvent(s string) (Event, error) {
	if ev := Event(s); slices.Contains(allEvents, ev) {
		return ev, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownEvent, s)
}

func ParseState(s string) (State, error) {
	if st := State(s); slices.Contains(allStates, st) {
		return st, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownState, s)
}
