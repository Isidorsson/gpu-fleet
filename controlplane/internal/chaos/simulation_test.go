package chaos

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

// The simulated platform: reservations take the lowest free nodes, a node
// with an injected XID is quarantined after the alert pipeline's delay, an
// implant is found when the node is wiped and re-attested after its
// reservation ends, and repair sends the node back through a wipe.
const (
	simTick       = 10 * time.Second
	simDuration   = 3 * time.Hour
	simNodes      = 20
	simAlertDelay = 90 * time.Second
	simWipeDelay  = 60 * time.Second
	// simInspectionFailOdds is 1 in N ticks.
	simInspectionFailOdds = 30
)

type simNode struct {
	state    node.State
	since    time.Time
	xidAt    time.Time
	tampered bool
}

func (n *simNode) faulted() bool {
	return n.state == node.StateQuarantined || !n.xidAt.IsZero() || n.tampered
}

type simReservation struct {
	info   ReservationInfo
	endsAt time.Time
}

type simWorld struct {
	nodes        map[string]*simNode
	reservations []*simReservation
}

func newSimWorld() *simWorld {
	w := &simWorld{nodes: map[string]*simNode{}}
	for i := 1; i <= simNodes; i++ {
		w.nodes[fmt.Sprintf("node-%03d", i)] = &simNode{state: node.StateAvailable, since: t0}
	}
	return w
}

func (w *simWorld) ids() []string {
	ids := make([]string, 0, len(w.nodes))
	for id := range w.nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (w *simWorld) advance(now time.Time) {
	for _, r := range w.reservations {
		if r.info.Status == ReservationActive && !now.Before(r.endsAt) {
			r.info.Status = "ended"
			for _, id := range r.info.Nodes {
				if n := w.nodes[id]; n.state == node.StateActive {
					n.state, n.since = node.StateWiping, now
				}
			}
		}
	}
	for _, n := range w.nodes {
		switch {
		case n.state != node.StateQuarantined && !n.xidAt.IsZero() && now.Sub(n.xidAt) >= simAlertDelay:
			n.state, n.since = node.StateQuarantined, now
		case n.state == node.StateWiping && now.Sub(n.since) >= simWipeDelay && n.tampered:
			n.state, n.since = node.StateQuarantined, now
		case n.state == node.StateWiping && now.Sub(n.since) >= simWipeDelay:
			n.state, n.since = node.StateAvailable, now
		}
	}
}

func (w *simWorld) snapshot() Snapshot {
	snap := Snapshot{AgentBacked: agentSet("node-001", "node-002", "node-003", "node-004")}
	for _, id := range w.ids() {
		snap.Nodes = append(snap.Nodes, NodeInfo{ID: id, State: w.nodes[id].state})
	}
	for _, r := range w.reservations {
		snap.Reservations = append(snap.Reservations, r.info)
	}
	return snap
}

func (w *simWorld) apply(a Action, now time.Time) {
	switch a.Kind {
	case KindReserve:
		w.reserve(a)
	case KindXID:
		w.nodes[a.Node].xidAt = now
	case KindImplant:
		w.nodes[a.Node].tampered = true
	case KindRepair:
		n := w.nodes[a.Node]
		n.xidAt, n.tampered = time.Time{}, false
		n.state, n.since = node.StateWiping, now
	}
}

func (w *simWorld) reserve(a Action) {
	var free []string
	for _, id := range w.ids() {
		if w.nodes[id].state == node.StateAvailable {
			free = append(free, id)
		}
	}
	if len(free) < a.Count {
		return
	}
	held := free[:a.Count]
	for _, id := range held {
		w.nodes[id].state = node.StateActive
	}
	w.reservations = append(w.reservations, &simReservation{
		info:   ReservationInfo{ID: a.Tenant, Tenant: a.Tenant, Status: ReservationActive, Nodes: held},
		endsAt: a.EndsAt,
	})
}

func (w *simWorld) faultedCount() int {
	count := 0
	for _, n := range w.nodes {
		if n.faulted() {
			count++
		}
	}
	return count
}

func TestSimulatedRunKeepsEveryBoundAndExercisesEveryAction(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			runSimulation(t, seed)
		})
	}
}

func runSimulation(t *testing.T, seed uint64) {
	t.Helper()
	w, st, rng := newSimWorld(), State{}, newRNG(seed)
	noise := newRNG(seed + 1000)
	times := map[Kind][]time.Time{}
	for now := t0; now.Before(t0.Add(simDuration)); now = now.Add(simTick) {
		w.advance(now)
		w.maybeFailInspection(noise, now)
		snap := w.snapshot()
		st = st.Observe(snap, now)
		a, ok := Plan(snap, st, now, rng)
		if !ok {
			continue
		}
		if faulting := a.Kind == KindXID || a.Kind == KindImplant; faulting && w.faultedCount() >= maxFaulted {
			t.Fatalf("at %s: %s with %d nodes already faulted or quarantined, bound is %d",
				now.Sub(t0), a, w.faultedCount(), maxFaulted)
		}
		w.apply(a, now)
		st = st.Attempted(a, now).Applied(a, now)
		times[a.Kind] = append(times[a.Kind], now)
	}

	assertSpacing(t, "xid", times[KindXID], xidInterval)
	assertSpacing(t, "implant", times[KindImplant], implantInterval)
	assertAtMostPerWindow(t, "xid", times[KindXID], maxXIDPerWindow, xidWindow)
	for _, kind := range []Kind{KindRepair, KindXID, KindImplant, KindReserve} {
		if len(times[kind]) < 3 {
			t.Errorf("%s fired %d times in %s; the demo would look idle", kind, len(times[kind]), simDuration)
		}
	}
}

func assertSpacing(t *testing.T, what string, times []time.Time, min time.Duration) {
	t.Helper()
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < min {
			t.Errorf("%s actions %s apart, minimum is %s", what, gap, min)
		}
	}
}

func assertAtMostPerWindow(t *testing.T, what string, times []time.Time, max int, window time.Duration) {
	t.Helper()
	for i := range times {
		inWindow := 0
		for _, other := range times {
			if d := times[i].Sub(other); d >= 0 && d < window {
				inWindow++
			}
		}
		if inWindow > max {
			t.Errorf("%d %s actions within %s of %s, bound is %d", inWindow, what, window, times[i].Sub(t0), max)
		}
	}
}

// maybeFailInspection quarantines a random available node now and then, like
// bmcsim's failed inspections: faults chaos did not cause still count against
// the bound.
func (w *simWorld) maybeFailInspection(noise *rand.Rand, now time.Time) {
	if noise.IntN(simInspectionFailOdds) != 0 {
		return
	}
	var free []string
	for _, id := range w.ids() {
		if w.nodes[id].state == node.StateAvailable {
			free = append(free, id)
		}
	}
	if len(free) == 0 {
		return
	}
	n := w.nodes[free[noise.IntN(len(free))]]
	n.state, n.since = node.StateQuarantined, now
}
