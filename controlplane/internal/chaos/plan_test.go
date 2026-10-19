package chaos

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
)

// twoOpen keeps the reserve rule quiet so a test sees the rule it targets.
func twoOpen() []ReservationInfo {
	return []ReservationInfo{resOf("s1", ReservationScheduled), resOf("s2", ReservationScheduled)}
}

func planAt(snap Snapshot, st State, seed uint64) (Action, bool) {
	return Plan(snap, st, t0, newRNG(seed))
}

func stateWithLast(kind Kind, at time.Time) State {
	return State{Last: map[Kind]time.Time{kind: at}}
}

func TestReserveFiresOnlyBelowTwoOpenReservations(t *testing.T) {
	tests := []struct {
		name string
		res  []ReservationInfo
		want bool
	}{
		{"none", nil, true},
		{"one active", []ReservationInfo{resOf("a", ReservationActive)}, true},
		{"one open and finished ones", []ReservationInfo{
			resOf("a", ReservationActive), resOf("b", "ended"), resOf("c", "cancelled"),
		}, true},
		{"scheduled and active", []ReservationInfo{resOf("a", ReservationScheduled), resOf("b", ReservationActive)}, false},
		{"degraded counts", []ReservationInfo{resOf("a", ReservationDegraded), resOf("b", ReservationScheduled)}, false},
		{"two open and finished ones", []ReservationInfo{
			resOf("a", ReservationActive), resOf("b", ReservationActive), resOf("c", "ended"),
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := planAt(snapOf(nil, tt.res), State{}, 1)
			if ok != tt.want {
				t.Fatalf("Plan ok = %v (%v), want %v", ok, a, tt.want)
			}
			if ok && a.Kind != KindReserve {
				t.Errorf("kind = %q, want reserve", a.Kind)
			}
		})
	}
}

func TestReserveParametersStayInRange(t *testing.T) {
	counts, minutes := map[int]bool{}, map[int]bool{}
	policies := map[placement.Policy]bool{}
	for seed := range uint64(400) {
		a, ok := planAt(Snapshot{}, State{}, seed)
		if !ok || a.Kind != KindReserve {
			t.Fatalf("seed %d: want reserve, got %v %v", seed, a, ok)
		}
		if a.Count < 4 || a.Count > 12 {
			t.Fatalf("seed %d: count %d outside 4..12", seed, a.Count)
		}
		d := a.EndsAt.Sub(a.StartsAt)
		if d < 3*time.Minute || d > 6*time.Minute || d%time.Minute != 0 {
			t.Fatalf("seed %d: duration %s is not 3..6 whole minutes", seed, d)
		}
		if !a.StartsAt.Equal(t0) {
			t.Fatalf("seed %d: starts at %s, want now", seed, a.StartsAt)
		}
		if !slices.Contains(reservePolicies, a.Policy) {
			t.Fatalf("seed %d: policy %q", seed, a.Policy)
		}
		counts[a.Count], minutes[int(d/time.Minute)], policies[a.Policy] = true, true, true
	}
	if len(counts) != 9 || len(minutes) != 4 || len(policies) != 3 {
		t.Errorf("the ranges are not fully used: %d counts, %d durations, %d policies", len(counts), len(minutes), len(policies))
	}
}

func TestReserveNumbersTenantsFromState(t *testing.T) {
	a, _ := planAt(Snapshot{}, State{}, 1)
	if a.Tenant != "tenant-1" {
		t.Errorf("first tenant = %q", a.Tenant)
	}
	a, _ = planAt(Snapshot{}, State{TenantsIssued: 6}, 1)
	if a.Tenant != "tenant-7" {
		t.Errorf("tenant after 6 issued = %q", a.Tenant)
	}
}

func implantWorld() Snapshot {
	return snapOf(
		[]NodeInfo{
			nodeOf("node-010", node.StateActive),
			nodeOf("node-011", node.StateActive),
			nodeOf("node-012", node.StateAvailable),
		},
		append(twoOpen()[:1], resOf("r1", ReservationActive, "node-010", "node-012")),
	)
}

func TestImplantTargetsOnlyActiveNodesOfActiveReservations(t *testing.T) {
	for seed := range uint64(100) {
		a, ok := planAt(implantWorld(), State{}, seed)
		if !ok || a.Kind != KindImplant {
			t.Fatalf("seed %d: want implant, got %v %v", seed, a, ok)
		}
		if a.Node != "node-010" {
			t.Fatalf("seed %d: implant on %s; node-011 is in no reservation and node-012 is not active", seed, a.Node)
		}
		if !slices.Contains(implantComponents, a.Component) {
			t.Fatalf("seed %d: component %q", seed, a.Component)
		}
	}
}

func TestImplantUsesEveryComponent(t *testing.T) {
	seen := map[string]bool{}
	for seed := range uint64(300) {
		a, _ := planAt(implantWorld(), State{}, seed)
		seen[a.Component] = true
	}
	if len(seen) != len(implantComponents) {
		t.Errorf("components seen = %v, want all of %v", seen, implantComponents)
	}
}

func TestImplantNeverTargetsAgentBackedNodes(t *testing.T) {
	snap := snapOf(
		[]NodeInfo{nodeOf("node-001", node.StateActive), nodeOf("node-010", node.StateActive)},
		[]ReservationInfo{resOf("r1", ReservationActive, "node-001", "node-010"), resOf("s", ReservationScheduled)},
		"node-001",
	)
	st := stateWithLast(KindXID, t0.Add(-time.Minute))
	for seed := range uint64(100) {
		a, ok := planAt(snap, st, seed)
		if !ok || a.Kind != KindImplant || a.Node != "node-010" {
			t.Fatalf("seed %d: want implant on node-010, got %v %v", seed, a, ok)
		}
	}

	onlyAgent := snapOf(
		[]NodeInfo{nodeOf("node-001", node.StateActive)},
		[]ReservationInfo{resOf("r1", ReservationActive, "node-001"), resOf("s", ReservationScheduled)},
		"node-001",
	)
	if a, ok := planAt(onlyAgent, st, 1); ok {
		t.Errorf("only an agent-backed node is reserved, got %v", a)
	}
}

func TestImplantNeedsAnActiveReservation(t *testing.T) {
	for _, status := range []ReservationStatus{ReservationScheduled, ReservationDegraded, "ended"} {
		snap := snapOf(
			[]NodeInfo{nodeOf("node-010", node.StateActive)},
			[]ReservationInfo{resOf("r1", status, "node-010"), resOf("s", ReservationScheduled), resOf("s2", ReservationScheduled)},
		)
		if a, ok := planAt(snap, State{}, 1); ok {
			t.Errorf("status %s: want no action, got %v", status, a)
		}
	}
}

func TestImplantInterval(t *testing.T) {
	tests := []struct {
		name string
		last time.Time
		want bool
	}{
		{"never", time.Time{}, true},
		{"just before 5 min", t0.Add(-implantInterval + time.Second), false},
		{"exactly 5 min", t0.Add(-implantInterval), true},
		{"long ago", t0.Add(-time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := planAt(implantWorld(), stateWithLast(KindImplant, tt.last), 1)
			if got := ok && a.Kind == KindImplant; got != tt.want {
				t.Errorf("implant = %v (%v), want %v", got, a, tt.want)
			}
		})
	}
}

func TestImplantSkipsNodesAlreadyImplanted(t *testing.T) {
	st := State{Pending: map[string]time.Time{"node-010": t0.Add(-time.Minute)}}
	if a, ok := planAt(implantWorld(), st, 1); ok && a.Kind == KindImplant {
		t.Errorf("node-010 is already implanted, got %v", a)
	}
}

func xidWorld() Snapshot {
	return snapOf(
		[]NodeInfo{
			nodeOf("node-001", node.StateActive),
			nodeOf("node-002", node.StateProvisioning),
			nodeOf("node-003", node.StateAvailable),
			nodeOf("node-010", node.StateActive),
		},
		twoOpen(),
		"node-001", "node-002", "node-003",
	)
}

func TestXIDTargetsActiveAgentBackedNodes(t *testing.T) {
	for seed := range uint64(100) {
		a, ok := planAt(xidWorld(), State{}, seed)
		if !ok || a.Kind != KindXID {
			t.Fatalf("seed %d: want xid, got %v %v", seed, a, ok)
		}
		if a.Node != "node-001" {
			t.Fatalf("seed %d: xid on %s; only node-001 is active and has an agent", seed, a.Node)
		}
		if a.GPU < 0 || a.GPU >= gpusPerAgent || !slices.Contains(xidCodes, a.Code) {
			t.Fatalf("seed %d: gpu %d code %d", seed, a.GPU, a.Code)
		}
	}
}

func TestXIDUsesEveryGPUAndCode(t *testing.T) {
	gpus, codes := map[int]bool{}, map[int]bool{}
	for seed := range uint64(300) {
		a, _ := planAt(xidWorld(), State{}, seed)
		gpus[a.GPU], codes[a.Code] = true, true
	}
	if len(gpus) != gpusPerAgent || len(codes) != 3 {
		t.Errorf("gpus %v, codes %v", gpus, codes)
	}
	for _, code := range []int{79, 48, 94} {
		if !codes[code] {
			t.Errorf("code %d never chosen", code)
		}
	}
}

func TestXIDNeedsAnActiveAgentBackedNode(t *testing.T) {
	snap := snapOf(
		[]NodeInfo{nodeOf("node-001", node.StateAvailable), nodeOf("node-010", node.StateActive)},
		twoOpen(),
		"node-001",
	)
	if a, ok := planAt(snap, State{}, 1); ok {
		t.Errorf("no agent-backed node is active, got %v", a)
	}
}

func TestXIDInterval(t *testing.T) {
	tests := []struct {
		name string
		last time.Time
		want bool
	}{
		{"never", time.Time{}, true},
		{"just before 4 min", t0.Add(-xidInterval + time.Second), false},
		{"exactly 4 min", t0.Add(-xidInterval), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := planAt(xidWorld(), stateWithLast(KindXID, tt.last), 1)
			if ok != tt.want {
				t.Errorf("ok = %v, want %v", ok, tt.want)
			}
		})
	}
}

func TestXIDRollingWindowCapsAtThreePerTenMinutes(t *testing.T) {
	at := func(mins ...float64) []time.Time {
		var out []time.Time
		for _, m := range mins {
			out = append(out, t0.Add(-time.Duration(m*float64(time.Minute))))
		}
		return out
	}
	tests := []struct {
		name  string
		times []time.Time
		want  bool
	}{
		{"two in the window", at(9, 7), true},
		{"three in the window", at(9, 7, 5), false},
		{"third has aged out at exactly 10 min", at(10, 7, 5), true},
		{"three just inside the window", at(9.9, 8, 6), false},
		{"older ones do not count", at(30, 20, 15, 12), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := State{XIDTimes: tt.times, Last: map[Kind]time.Time{KindXID: t0.Add(-xidInterval)}}
			_, ok := planAt(xidWorld(), st, 1)
			if ok != tt.want {
				t.Errorf("ok = %v, want %v", ok, tt.want)
			}
		})
	}
}

func TestXIDSkipsNodesAlreadyFaulted(t *testing.T) {
	st := State{Pending: map[string]time.Time{"node-001": t0.Add(-time.Minute)}}
	if a, ok := planAt(xidWorld(), st, 1); ok {
		t.Errorf("node-001 already has a fault pending, got %v", a)
	}
	expired := State{Pending: map[string]time.Time{"node-001": t0.Add(-pendingTTL)}}
	if a, ok := planAt(xidWorld(), expired, 1); !ok || a.Kind != KindXID {
		t.Errorf("a stale pending entry must not block, got %v %v", a, ok)
	}
}

func quarantinedFor(d time.Duration, ids ...string) (nodes []NodeInfo, st State) {
	st.QuarantinedSince = map[string]time.Time{}
	for _, id := range ids {
		nodes = append(nodes, nodeOf(id, node.StateQuarantined))
		st.QuarantinedSince[id] = t0.Add(-d)
	}
	return nodes, st
}

func TestRepairWaitsTwoMinutes(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"just quarantined", 0, false},
		{"just under 2 min", repairAfter - time.Second, false},
		{"exactly 2 min", repairAfter, true},
		{"long quarantined", time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes, st := quarantinedFor(tt.age, "node-005")
			a, ok := planAt(snapOf(nodes, twoOpen()), st, 1)
			if ok != tt.want {
				t.Fatalf("ok = %v (%v), want %v", ok, a, tt.want)
			}
			if ok && (a.Kind != KindRepair || a.Node != "node-005") {
				t.Errorf("got %v", a)
			}
		})
	}
}

func TestRepairIgnoresNodesThatAreNotQuarantinedOrNotObserved(t *testing.T) {
	st := State{QuarantinedSince: map[string]time.Time{"node-005": t0.Add(-time.Hour)}}
	stale := snapOf([]NodeInfo{nodeOf("node-005", node.StateWiping)}, twoOpen())
	if a, ok := planAt(stale, st, 1); ok {
		t.Errorf("node left quarantine, got %v", a)
	}
	unseen := snapOf([]NodeInfo{nodeOf("node-006", node.StateQuarantined)}, twoOpen())
	if a, ok := planAt(unseen, st, 1); ok {
		t.Errorf("no observation time for node-006, got %v", a)
	}
}

func TestRepairPicksTheLongestQuarantinedThenLowestID(t *testing.T) {
	nodes, st := quarantinedFor(5*time.Minute, "node-009", "node-003")
	nodes = append(nodes, nodeOf("node-004", node.StateQuarantined), nodeOf("node-007", node.StateQuarantined))
	st.QuarantinedSince["node-004"] = t0.Add(-9 * time.Minute)
	st.QuarantinedSince["node-007"] = t0.Add(-7 * time.Minute)
	a, _ := planAt(snapOf(nodes, twoOpen()), st, 1)
	if a.Node != "node-004" {
		t.Errorf("longest quarantined is node-004, got %v", a)
	}

	nodes, st = quarantinedFor(5*time.Minute, "node-009", "node-003", "node-006")
	a, _ = planAt(snapOf(nodes, twoOpen()), st, 1)
	if a.Node != "node-003" {
		t.Errorf("tie breaks to the lowest ID, got %v", a)
	}
}

func TestRepairIsNotBoundedByTheFaultCap(t *testing.T) {
	nodes, st := quarantinedFor(10*time.Minute, "node-001", "node-002", "node-003", "node-004")
	a, ok := planAt(snapOf(nodes, twoOpen()), st, 1)
	if !ok || a.Kind != KindRepair {
		t.Errorf("repair must run with 4 quarantined, got %v %v", a, ok)
	}
}

func capWorld(quarantined ...string) (Snapshot, State) {
	nodes, st := quarantinedFor(30*time.Second, quarantined...)
	nodes = append(nodes, nodeOf("node-001", node.StateActive), nodeOf("node-010", node.StateActive))
	reservations := []ReservationInfo{resOf("r1", ReservationActive, "node-010"), resOf("s", ReservationScheduled)}
	return snapOf(nodes, reservations, "node-001"), st
}

func TestFaultCapBlocksXIDAndImplantAtThree(t *testing.T) {
	snap, st := capWorld("node-020", "node-021", "node-022")
	if a, ok := planAt(snap, st, 1); ok {
		t.Errorf("3 quarantined: no new fault allowed, got %v", a)
	}
	snap, st = capWorld("node-020", "node-021")
	if a, ok := planAt(snap, st, 1); !ok || a.Kind != KindXID {
		t.Errorf("2 quarantined: a third fault is allowed, got %v %v", a, ok)
	}
	st.Last = map[Kind]time.Time{KindXID: t0.Add(-time.Minute)}
	if a, ok := planAt(snap, st, 1); !ok || a.Kind != KindImplant {
		t.Errorf("2 quarantined, xid on cooldown: implant allowed, got %v %v", a, ok)
	}
}

func TestFaultCapCountsPendingFaultsOnce(t *testing.T) {
	tests := []struct {
		name        string
		quarantined []string
		pending     map[string]time.Time
		want        bool
	}{
		{"2 quarantined + 1 pending", []string{"node-020", "node-021"}, map[string]time.Time{"node-030": t0.Add(-time.Minute)}, false},
		{"1 quarantined + 2 pending", []string{"node-020"}, map[string]time.Time{"node-030": t0.Add(-time.Minute), "node-031": t0.Add(-time.Minute)}, false},
		{"pending that is also quarantined counts once", []string{"node-020", "node-021"}, map[string]time.Time{"node-021": t0.Add(-time.Minute)}, true},
		{"stale pending is forgotten", []string{"node-020", "node-021"}, map[string]time.Time{"node-030": t0.Add(-pendingTTL)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, st := capWorld(tt.quarantined...)
			st.Pending = tt.pending
			_, ok := planAt(snap, st, 1)
			if ok != tt.want {
				t.Errorf("ok = %v, want %v", ok, tt.want)
			}
		})
	}
}

func TestFaultCapDoesNotBlockReserve(t *testing.T) {
	nodes, st := quarantinedFor(30*time.Second, "node-020", "node-021", "node-022")
	a, ok := planAt(snapOf(nodes, nil), st, 1)
	if !ok || a.Kind != KindReserve {
		t.Errorf("reserve is not a fault, got %v %v", a, ok)
	}
}

func TestPriorityIsRepairXIDImplantReserve(t *testing.T) {
	nodes, st := quarantinedFor(3*time.Minute, "node-020")
	nodes = append(nodes, nodeOf("node-001", node.StateActive), nodeOf("node-010", node.StateActive))
	reservations := []ReservationInfo{resOf("r1", ReservationActive, "node-010")}
	snap := snapOf(nodes, reservations, "node-001")

	steps := []struct {
		name   string
		mutate func(*Snapshot, *State)
		want   Kind
	}{
		{"everything applies", func(*Snapshot, *State) {}, KindRepair},
		{"quarantine too young to repair", func(_ *Snapshot, st *State) {
			st.QuarantinedSince["node-020"] = t0.Add(-time.Minute)
		}, KindXID},
		{"xid on cooldown", func(_ *Snapshot, st *State) {
			st.Last = map[Kind]time.Time{KindXID: t0.Add(-time.Minute)}
		}, KindImplant},
		{"implant on cooldown", func(_ *Snapshot, st *State) {
			st.Last[KindImplant] = t0.Add(-time.Minute)
		}, KindReserve},
	}
	for _, step := range steps {
		step.mutate(&snap, &st)
		a, ok := planAt(snap, st, 1)
		if !ok || a.Kind != step.want {
			t.Fatalf("%s: got %v %v, want %s", step.name, a, ok, step.want)
		}
	}

	snap.Reservations = append(snap.Reservations, resOf("s", ReservationScheduled))
	if a, ok := planAt(snap, st, 1); ok {
		t.Errorf("nothing applies, got %v", a)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	worlds := map[string]Snapshot{
		"reserve": {},
		"xid":     xidWorld(),
		"implant": implantWorld(),
	}
	for name, snap := range worlds {
		for seed := range uint64(50) {
			a1, ok1 := planAt(snap, State{}, seed)
			a2, ok2 := planAt(snap, State{}, seed)
			if a1 != a2 || ok1 != ok2 {
				t.Fatalf("%s seed %d: %v vs %v", name, seed, a1, a2)
			}
		}
	}
}

func TestPlanIgnoresTheOrderTheAPIListsThings(t *testing.T) {
	snap := implantWorld()
	reversed := Snapshot{
		Nodes:        slices.Clone(snap.Nodes),
		Reservations: slices.Clone(snap.Reservations),
		AgentBacked:  snap.AgentBacked,
	}
	slices.Reverse(reversed.Nodes)
	slices.Reverse(reversed.Reservations)
	for seed := range uint64(50) {
		a1, _ := planAt(snap, State{}, seed)
		a2, _ := planAt(reversed, State{}, seed)
		if a1 != a2 {
			t.Fatalf("seed %d: %v vs %v", seed, a1, a2)
		}
	}
}

func TestPlanVariesWithTheSeed(t *testing.T) {
	seen := map[Action]bool{}
	for seed := range uint64(30) {
		a, _ := planAt(Snapshot{}, State{}, seed)
		seen[a] = true
	}
	if len(seen) < 10 {
		t.Errorf("30 seeds gave only %d distinct reservations", len(seen))
	}
}

func TestPlanDoesNotMutateItsInputs(t *testing.T) {
	snap, st := capWorld("node-020")
	st.Pending = map[string]time.Time{"node-030": t0.Add(-time.Minute)}
	st.XIDTimes = []time.Time{t0.Add(-time.Minute)}
	st.Last = map[Kind]time.Time{KindImplant: t0.Add(-time.Hour)}
	wantSnap := Snapshot{
		Nodes:        slices.Clone(snap.Nodes),
		Reservations: slices.Clone(snap.Reservations),
		AgentBacked:  agentSet("node-001"),
	}
	wantState := st.clone()

	for seed := range uint64(20) {
		planAt(snap, st, seed)
	}
	if !reflect.DeepEqual(snap, wantSnap) {
		t.Errorf("snapshot changed: %+v", snap)
	}
	if !reflect.DeepEqual(st, wantState) {
		t.Errorf("state changed: %+v", st)
	}
}

func TestPlanDrawsNothingWhenIdle(t *testing.T) {
	rng := newRNG(9)
	if a, ok := Plan(snapOf(nil, twoOpen()), State{}, t0, rng); ok {
		t.Fatalf("want idle, got %v", a)
	}
	fresh := newRNG(9)
	if rng.Uint64() != fresh.Uint64() {
		t.Error("an idle tick consumed random numbers, so the stream would depend on idle ticks")
	}
}

func TestActionStringsDescribeEachKind(t *testing.T) {
	tests := []struct {
		a    Action
		want string
	}{
		{Action{Kind: KindRepair, Node: "node-005"}, "repair node-005"},
		{Action{Kind: KindXID, Node: "node-001", GPU: 3, Code: 79}, "xid 79 on node-001 gpu 3"},
		{Action{Kind: KindImplant, Node: "node-010", Component: "kernel"}, "implant kernel on node-010"},
		{Action{
			Kind: KindReserve, Tenant: "tenant-2", Count: 8, Policy: placement.PolicySamePod,
			StartsAt: t0, EndsAt: t0.Add(4 * time.Minute),
		}, "reserve 8 nodes same_pod for tenant-2 for 4m0s"},
	}
	for _, tt := range tests {
		if got := tt.a.String(); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}
