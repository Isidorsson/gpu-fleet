package chaos

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
)

// Bounds from docs/demo.md. The rate bounds keep chaos under the
// remediation limit (5 quarantines per 10 minutes) and the fleet usable.
const (
	maxFaulted      = 3
	maxXIDPerWindow = 3
	xidWindow       = 10 * time.Minute
	xidInterval     = 4 * time.Minute
	implantInterval = 5 * time.Minute
	repairAfter     = 2 * time.Minute

	// pendingTTL stops a fault that never led to a quarantine (an alert that
	// was dropped, a reservation that ended first) from holding a slot of
	// maxFaulted forever. An implant is caught after its reservation ends and
	// the node is wiped and re-attested, which takes well under this.
	pendingTTL = 15 * time.Minute

	minOpenReservations = 2
	minReserveCount     = 4
	maxReserveCount     = 12
	minReserveMinutes   = 3
	maxReserveMinutes   = 6

	// gpusPerAgent is the agent's default FAKE_GPUS; a higher index is a 400.
	gpusPerAgent = 8

	tenantPrefix = "tenant-"
)

var (
	xidCodes          = []int{79, 48, 94}
	implantComponents = []string{"firmware", "bmc", "bootloader", "kernel", "gpu_vbios", "cc_mode"}
	reservePolicies   = []placement.Policy{placement.PolicySameRack, placement.PolicySamePod, placement.PolicyAny}
)

// Action is one thing chaos does to the platform. Which fields are set
// depends on Kind: Node for repair, xid and implant; GPU and Code for xid;
// Component for implant; the rest for reserve.
type Action struct {
	Kind      Kind
	Node      string
	GPU       int
	Code      int
	Component string
	Tenant    string
	Count     int
	Policy    placement.Policy
	StartsAt  time.Time
	EndsAt    time.Time
}

func (a Action) String() string {
	switch a.Kind {
	case KindRepair:
		return fmt.Sprintf("repair %s", a.Node)
	case KindXID:
		return fmt.Sprintf("xid %d on %s gpu %d", a.Code, a.Node, a.GPU)
	case KindImplant:
		return fmt.Sprintf("implant %s on %s", a.Component, a.Node)
	case KindReserve:
		return fmt.Sprintf("reserve %d nodes %s for %s for %s", a.Count, a.Policy, a.Tenant, a.EndsAt.Sub(a.StartsAt))
	}
	return string(a.Kind)
}

type planner func(Snapshot, State, time.Time, *rand.Rand) (Action, bool)

// priority is the order docs/demo.md gives for when several actions apply.
var priority = []planner{planRepair, planXID, planImplant, planReserve}

// Plan chooses at most one action. It is pure: it reads its arguments, never
// mutates them, and draws from rng only once it has decided to act, so the
// random stream depends on the actions taken and nothing else. The state must
// have been through State.Observe for this snapshot.
func Plan(snap Snapshot, st State, now time.Time, rng *rand.Rand) (Action, bool) {
	for _, plan := range priority {
		if a, ok := plan(snap, st, now, rng); ok {
			return a, true
		}
	}
	return Action{}, false
}

// planRepair picks the node quarantined the longest, ties by ID. It ignores
// every bound: repairing only ever reduces the number of faulted nodes.
func planRepair(snap Snapshot, st State, now time.Time, _ *rand.Rand) (Action, bool) {
	var (
		best      string
		bestSince time.Time
	)
	for _, n := range snap.Nodes {
		since, seen := st.QuarantinedSince[n.ID]
		if n.State != node.StateQuarantined || !seen || now.Sub(since) < repairAfter {
			continue
		}
		if best == "" || since.Before(bestSince) || (since.Equal(bestSince) && n.ID < best) {
			best, bestSince = n.ID, since
		}
	}
	return Action{Kind: KindRepair, Node: best}, best != ""
}

func planXID(snap Snapshot, st State, now time.Time, rng *rand.Rand) (Action, bool) {
	if !hasFaultRoom(snap, st, now) || !due(st.Last[KindXID], now, xidInterval) ||
		st.xidsInWindow(now) >= maxXIDPerWindow {
		return Action{}, false
	}
	targets := xidTargets(snap, st, now)
	if len(targets) == 0 {
		return Action{}, false
	}
	return Action{
		Kind: KindXID,
		Node: targets[rng.IntN(len(targets))],
		GPU:  rng.IntN(gpusPerAgent),
		Code: xidCodes[rng.IntN(len(xidCodes))],
	}, true
}

func planImplant(snap Snapshot, st State, now time.Time, rng *rand.Rand) (Action, bool) {
	if !hasFaultRoom(snap, st, now) || !due(st.Last[KindImplant], now, implantInterval) {
		return Action{}, false
	}
	targets := implantTargets(snap, st, now)
	if len(targets) == 0 {
		return Action{}, false
	}
	return Action{
		Kind:      KindImplant,
		Node:      targets[rng.IntN(len(targets))],
		Component: implantComponents[rng.IntN(len(implantComponents))],
	}, true
}

func planReserve(snap Snapshot, st State, now time.Time, rng *rand.Rand) (Action, bool) {
	open := 0
	for _, r := range snap.Reservations {
		if r.open() {
			open++
		}
	}
	if open >= minOpenReservations {
		return Action{}, false
	}
	policy := reservePolicies[rng.IntN(len(reservePolicies))]
	count := minReserveCount + rng.IntN(maxReserveCount-minReserveCount+1)
	minutes := minReserveMinutes + rng.IntN(maxReserveMinutes-minReserveMinutes+1)
	return Action{
		Kind:     KindReserve,
		Tenant:   fmt.Sprintf("%s%d", tenantPrefix, st.TenantsIssued+1),
		Count:    count,
		Policy:   policy,
		StartsAt: now,
		EndsAt:   now.Add(time.Duration(minutes) * time.Minute),
	}, true
}

// hasFaultRoom is the maxFaulted bound: nodes quarantined now plus nodes chaos
// faulted that are not quarantined yet, each counted once. A new fault needs
// a free slot.
func hasFaultRoom(snap Snapshot, st State, now time.Time) bool {
	faulted := map[string]bool{}
	for _, n := range snap.Nodes {
		if n.State == node.StateQuarantined {
			faulted[n.ID] = true
		}
	}
	for id := range st.Pending {
		if st.isPending(id, now) {
			faulted[id] = true
		}
	}
	return len(faulted) < maxFaulted
}

// xidTargets are the active nodes that have an agent, sorted so the random
// pick does not depend on the order the API listed them in.
func xidTargets(snap Snapshot, st State, now time.Time) []string {
	var ids []string
	for _, n := range snap.Nodes {
		if n.State == node.StateActive && snap.AgentBacked[n.ID] && !st.isPending(n.ID, now) {
			ids = append(ids, n.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// implantTargets are the active nodes of active reservations that have no
// agent: an implant is caught by attestation after the wipe, and an agent
// node is reserved for the xid path.
func implantTargets(snap Snapshot, st State, now time.Time) []string {
	eligible := map[string]bool{}
	for _, n := range snap.Nodes {
		if n.State == node.StateActive && !snap.AgentBacked[n.ID] && !st.isPending(n.ID, now) {
			eligible[n.ID] = true
		}
	}
	targets := map[string]bool{}
	for _, r := range snap.Reservations {
		if r.Status != ReservationActive {
			continue
		}
		for _, id := range r.Nodes {
			if eligible[id] {
				targets[id] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(targets))
}
