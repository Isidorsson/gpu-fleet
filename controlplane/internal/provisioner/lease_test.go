package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/redfish"
	"gpu-fleet/controlplane/internal/store"
)

func TestLeaseLostWhilePollingAbandonsNode(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, running)
	h.store.onRenew = func(call int) {
		if call == 2 {
			h.store.loseLease(testNodeID)
		}
	}

	worked, err := h.worker.Step(context.Background())

	if !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertSlice(t, "releases", h.store.releases, nil)
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateInspecting)
	assertEqual(t, "status polls", h.machine.count("status"), 1)
	assertEqual(t, "force offs", h.machine.count("reset:ForceOff"), 0)
}

func TestLeaseLostDuringRetryBackoffAbandonsNode(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.failNext("pxe_once", redfish.ErrTransient)
	h.store.onRenew = func(int) { h.store.loseLease(testNodeID) }

	worked, err := h.worker.Step(context.Background())

	if !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
	assertEqual(t, "attempts", h.machine.count("pxe_once"), 1)
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertSlice(t, "releases", h.store.releases, nil)
}

func TestLeaseLostBeforePowerOffSkipsPowerOff(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.statusFn = func() deployagent.Status {
		h.store.loseLease(testNodeID)
		return deployagent.Status{Task: deployagent.TaskInspect, Phase: succeeded}
	}

	worked, err := h.worker.Step(context.Background())

	if !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
	assertEqual(t, "force offs", h.machine.count("reset:ForceOff"), 0)
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertSlice(t, "releases", h.store.releases, nil)
}

func TestLeaseLostOnApplyDoesNotRelease(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.store.applyErr = store.ErrLeaseLost

	worked, err := h.worker.Step(context.Background())

	if !worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
	}
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateInspecting)
	assertSlice(t, "releases", h.store.releases, nil)
}

func TestOperatorChangeMidTaskWins(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
	}{
		{"task result no longer applies", func(h *harness) {
			h.machine.script(deployagent.TaskInspect, running, succeeded)
		}},
		{"fault no longer applies", func(h *harness) {
			h.machine.failNext("status", deployagent.ErrNotFound)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.add(nodeIn(node.StateInspecting, ""))
			tc.setup(h)
			h.store.onRenew = func(call int) {
				if call == 1 {
					h.store.operatorApply(testNodeID, node.EventHardwareFault)
				}
			}

			worked, err := h.worker.Step(context.Background())

			if !worked || err != nil {
				t.Fatalf("Step = (%v, %v), want (true, nil)", worked, err)
			}
			assertEqual(t, "state", h.store.get(testNodeID).State, node.StateQuarantined)
			assertSlice(t, "events", h.store.appliedEvents(), nil)
			assertReleasedOnce(t, h)
		})
	}
}

func TestShutdownMidTaskReleasesLeaseWithFreshContext(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, running)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.clock.onSleep = func(count int, _ time.Duration) {
		if count == 2 {
			cancel()
		}
	}

	worked, err := h.worker.Step(ctx)

	if !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("Step = (%v, %v), want (true, context canceled)", worked, err)
	}
	assertSlice(t, "events", h.store.appliedEvents(), nil)
	assertReleasedOnce(t, h)
}

func TestStoreFailureReleasesLeaseAndReturnsError(t *testing.T) {
	h := newHarness(t)
	h.store.add(nodeIn(node.StateInspecting, ""))
	h.machine.script(deployagent.TaskInspect, succeeded)
	boom := errors.New("db down")
	h.store.applyErr = boom

	worked, err := h.worker.Step(context.Background())

	if !worked || !errors.Is(err, boom) {
		t.Fatalf("Step = (%v, %v), want (true, db down)", worked, err)
	}
	assertEqual(t, "state", h.store.get(testNodeID).State, node.StateInspecting)
	assertReleasedOnce(t, h)
}

func TestClaimedNodeWithoutWorkIsReleased(t *testing.T) {
	h := newHarness(t)
	idle := nodeIn(node.StateAvailable, "")
	h.store.add(idle)
	h.worker.store = &claimOnceStore{fakeStore: h.store, n: idle}

	h.step(t)

	assertSlice(t, "bmc calls", h.machine.callLog(), nil)
	assertReleasedOnce(t, h)
}

// claimOnceStore hands out a fixed node on the first claim, regardless of
// state, to model a store that returned something the worker has no work for.
type claimOnceStore struct {
	*fakeStore
	n    node.Node
	done bool
}

func (c *claimOnceStore) ClaimWork(_ context.Context, owner string, _ time.Duration) (node.Node, error) {
	if c.done {
		return node.Node{}, store.ErrNoWork
	}
	c.done = true
	c.leases[c.n.ID] = owner
	return c.n, nil
}
