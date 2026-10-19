package chaos

import (
	"reflect"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

func TestObserveTimestampsNewlyQuarantinedNodesOnce(t *testing.T) {
	snap := snapOf([]NodeInfo{nodeOf("node-005", node.StateQuarantined), nodeOf("node-006", node.StateActive)}, nil)

	first := State{}.Observe(snap, t0)
	if got := first.QuarantinedSince["node-005"]; !got.Equal(t0) {
		t.Fatalf("first seen at %s, want %s", got, t0)
	}
	if _, ok := first.QuarantinedSince["node-006"]; ok {
		t.Error("an active node is not quarantined")
	}

	later := first.Observe(snap, t0.Add(time.Minute))
	if got := later.QuarantinedSince["node-005"]; !got.Equal(t0) {
		t.Errorf("a later sighting moved the timestamp to %s", got)
	}
}

func TestObserveForgetsNodesThatLeftQuarantine(t *testing.T) {
	st := State{}.Observe(snapOf([]NodeInfo{nodeOf("node-005", node.StateQuarantined)}, nil), t0)
	st = st.Observe(snapOf([]NodeInfo{nodeOf("node-005", node.StateWiping)}, nil), t0.Add(time.Minute))
	if len(st.QuarantinedSince) != 0 {
		t.Fatalf("still tracking %v", st.QuarantinedSince)
	}

	st = st.Observe(snapOf([]NodeInfo{nodeOf("node-005", node.StateQuarantined)}, nil), t0.Add(time.Hour))
	if got := st.QuarantinedSince["node-005"]; !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("a second quarantine starts a new clock, got %s", got)
	}
}

func TestObserveDropsPendingFaultsOnceQuarantinedOrStale(t *testing.T) {
	st := State{Pending: map[string]time.Time{
		"quarantined": t0.Add(-time.Minute),
		"waiting":     t0.Add(-time.Minute),
		"stale":       t0.Add(-pendingTTL),
	}}
	snap := snapOf([]NodeInfo{nodeOf("quarantined", node.StateQuarantined), nodeOf("waiting", node.StateActive)}, nil)

	got := st.Observe(snap, t0)
	want := map[string]time.Time{"waiting": t0.Add(-time.Minute)}
	if !reflect.DeepEqual(got.Pending, want) {
		t.Errorf("pending = %v, want %v", got.Pending, want)
	}
}

func TestObserveDropsXIDsOutsideTheWindow(t *testing.T) {
	st := State{XIDTimes: []time.Time{t0.Add(-xidWindow), t0.Add(-xidWindow + time.Second), t0.Add(-time.Minute)}}
	got := st.Observe(Snapshot{}, t0)
	if len(got.XIDTimes) != 2 {
		t.Errorf("kept %v, want the 2 inside the window", got.XIDTimes)
	}
}

func TestAttemptedRecordsEveryKindAndCountsXIDsAndTenants(t *testing.T) {
	st := State{}
	st = st.Attempted(Action{Kind: KindImplant, Node: "n"}, t0)
	st = st.Attempted(Action{Kind: KindXID, Node: "n"}, t0.Add(time.Minute))
	st = st.Attempted(Action{Kind: KindReserve}, t0.Add(2*time.Minute))
	st = st.Attempted(Action{Kind: KindReserve}, t0.Add(3*time.Minute))

	wantLast := map[Kind]time.Time{
		KindImplant: t0, KindXID: t0.Add(time.Minute), KindReserve: t0.Add(3 * time.Minute),
	}
	if !reflect.DeepEqual(st.Last, wantLast) {
		t.Errorf("last = %v", st.Last)
	}
	if len(st.XIDTimes) != 1 {
		t.Errorf("xid times = %v", st.XIDTimes)
	}
	if st.TenantsIssued != 2 {
		t.Errorf("tenants issued = %d, want 2", st.TenantsIssued)
	}
	if len(st.Pending) != 0 {
		t.Errorf("an attempt alone must not mark a fault pending: %v", st.Pending)
	}
}

func TestAppliedTracksFaultsAndClearsRepairs(t *testing.T) {
	st := State{}
	st = st.Applied(Action{Kind: KindXID, Node: "node-001"}, t0)
	st = st.Applied(Action{Kind: KindImplant, Node: "node-010"}, t0.Add(time.Minute))
	st = st.Applied(Action{Kind: KindReserve}, t0)
	want := map[string]time.Time{"node-001": t0, "node-010": t0.Add(time.Minute)}
	if !reflect.DeepEqual(st.Pending, want) {
		t.Fatalf("pending = %v, want %v", st.Pending, want)
	}

	st.QuarantinedSince = map[string]time.Time{"node-001": t0}
	st = st.Applied(Action{Kind: KindRepair, Node: "node-001"}, t0.Add(time.Hour))
	if _, ok := st.Pending["node-001"]; ok {
		t.Error("a repaired node is no longer pending")
	}
	if _, ok := st.QuarantinedSince["node-001"]; ok {
		t.Error("a repaired node is no longer quarantined")
	}
	if _, ok := st.Pending["node-010"]; !ok {
		t.Error("repairing one node must not forget another")
	}
}

func TestStateMethodsNeverChangeTheReceiver(t *testing.T) {
	st := State{
		Last:             map[Kind]time.Time{KindXID: t0},
		XIDTimes:         []time.Time{t0},
		Pending:          map[string]time.Time{"a": t0},
		QuarantinedSince: map[string]time.Time{"b": t0},
		TenantsIssued:    3,
	}
	want := st.clone()
	snap := snapOf([]NodeInfo{nodeOf("c", node.StateQuarantined)}, nil)

	st.Observe(snap, t0.Add(time.Hour))
	st.Attempted(Action{Kind: KindXID}, t0.Add(time.Hour))
	st.Attempted(Action{Kind: KindReserve}, t0.Add(time.Hour))
	st.Applied(Action{Kind: KindXID, Node: "d"}, t0.Add(time.Hour))
	st.Applied(Action{Kind: KindRepair, Node: "a"}, t0.Add(time.Hour))
	if !reflect.DeepEqual(st, want) {
		t.Errorf("receiver changed: %+v", st)
	}
}

func TestZeroStateIsUsable(t *testing.T) {
	var st State
	st = st.Observe(Snapshot{}, t0)
	st = st.Attempted(Action{Kind: KindXID}, t0)
	st = st.Applied(Action{Kind: KindXID, Node: "n"}, t0)
	if !st.isPending("n", t0) {
		t.Error("a node faulted on a zero state must be pending")
	}
}

func TestObserveMovesTheTenantCounterPastExistingTenants(t *testing.T) {
	snap := snapOf(nil, []ReservationInfo{
		{ID: "a", Tenant: "tenant-3"},
		{ID: "b", Tenant: "tenant-12"},
		{ID: "c", Tenant: "acme"},
		{ID: "d", Tenant: "tenant-x"},
		{ID: "e", Tenant: "tenant-"},
		{ID: "f", Tenant: "12345"},
		{ID: "g", Tenant: "tenant--7"},
	})
	st := State{}.Observe(snap, t0)
	if st.TenantsIssued != 12 {
		t.Fatalf("tenants issued = %d, want 12", st.TenantsIssued)
	}
	if a, _ := Plan(Snapshot{}, st, t0, newRNG(1)); a.Tenant != "tenant-13" {
		t.Errorf("the next tenant is %q, want tenant-13", a.Tenant)
	}

	ahead := State{TenantsIssued: 20}.Observe(snap, t0)
	if ahead.TenantsIssued != 20 {
		t.Errorf("the counter must never go back, got %d", ahead.TenantsIssued)
	}
}
