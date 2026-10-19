package reservation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

func TestStartProvisionsAvailableNodesWithTenant(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateAvailable, "")
	h.nodes.set("n2", node.StateAvailable, "")

	h.tick(t)

	assertEvents(t, h, provisioned("n1"), provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
	assertEqual(t, "n1 state", h.nodes.get("n1").State, node.StateProvisioning)
	assertEqual(t, "n1 tenant", h.nodes.get("n1").Tenant, testTenant)
	assertEqual(t, "n1 holder", h.nodes.get("n1").Reservation, "res-1")
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 0)
}

func TestStartIgnoresReservationsThatAreNotDue(t *testing.T) {
	h := newHarness(t)
	h.store.add(Reservation{
		ID: "res-future", Tenant: testTenant, Status: StatusScheduled, NodeIDs: []string{"n1"},
		StartsAt: testStart.Add(time.Minute), EndsAt: testStart.Add(time.Hour),
	})
	h.nodes.set("n1", node.StateAvailable, "")

	h.tick(t)

	assertEvents(t, h)
	assertEqual(t, "status", h.status("res-future"), StatusScheduled)
}

func TestStartWaitsForNodesThatAreNotReady(t *testing.T) {
	tests := []struct {
		name   string
		state  node.State
		tenant string
	}{
		{"racked", node.StateRacked, ""},
		{"inspecting", node.StateInspecting, ""},
		{"wiping", node.StateWiping, ""},
		{"provisioning for another tenant", node.StateProvisioning, otherTenant},
		{"active for another tenant", node.StateActive, otherTenant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.reserve("res-1", StatusScheduled, "n1", "n2")
			h.nodes.set("n1", node.StateAvailable, "")
			h.nodes.set("n2", tt.state, tt.tenant)

			h.tick(t)

			assertEvents(t, h, provisioned("n1"))
			assertEqual(t, "status while waiting", h.status("res-1"), StatusScheduled)
		})
	}
}

func TestStartCompletesOnALaterTickWithoutRepeatingWork(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateAvailable, "")
	h.nodes.set("n2", node.StateInspecting, "")

	h.tick(t)
	h.tick(t)
	assertEqual(t, "status while n2 inspects", h.status("res-1"), StatusScheduled)

	h.nodes.set("n2", node.StateAvailable, "")
	h.tick(t)

	assertEvents(t, h, provisioned("n1"), provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
}

func TestStartReplacesQuarantinedNodeAndProvisionsReplacement(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateAvailable, "")
	h.nodes.set("n3", node.StateAvailable, "")
	h.store.replacements["n1"] = "n3"

	h.tick(t)

	if len(h.store.replaceCalls) != 1 {
		t.Fatalf("ReplaceNode calls = %v, want 1", h.store.replaceCalls)
	}
	call := h.store.replaceCalls[0]
	assertEqual(t, "replaced reservation", call.ReservationID, "res-1")
	assertEqual(t, "replaced node", call.OldNode, "n1")
	assertEqual(t, "replace time", call.Now, testStart)
	assertEqual(t, "placer passed through", strings.Join(call.PlacerResult, ","), placerMarker)
	assertEvents(t, h, provisioned("n3"), provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 0)
}

func TestStartWaitsWhenReplacementIsNotReady(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateWiping, "")
	h.store.replacements["n1"] = "n2"

	h.tick(t)
	assertEvents(t, h)
	assertEqual(t, "status", h.status("res-1"), StatusScheduled)

	h.nodes.set("n2", node.StateAvailable, "")
	h.tick(t)

	assertEvents(t, h, provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
	assertEqual(t, "replace calls", len(h.store.replaceCalls), 1)
}

func TestStartDegradesWhenNoReplacementFits(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2", "n3")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateAvailable, "")
	h.nodes.set("n3", node.StateAvailable, "")

	h.tick(t)

	assertEvents(t, h, provisioned("n2"), provisioned("n3"))
	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
	assertEqual(t, "quarantined node untouched", h.nodes.get("n1").State, node.StateQuarantined)
	assertEqual(t, "dropped", strings.Join(h.store.drops, ","), "n1")
	assertEqual(t, "nodes left", strings.Join(h.store.get("res-1").NodeIDs, ","), "n2,n3")

	h.tick(t)
	assertEqual(t, "degraded count after another tick", h.metrics.degradedCount(), 1)
}

func TestDegradedIsCountedOncePerReservationAcrossRetries(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateRacked, "")

	h.tick(t)
	h.tick(t)
	assertEqual(t, "degraded while still scheduled", h.metrics.degradedCount(), 0)
	assertEqual(t, "replace attempts", len(h.store.replaceCalls), 2)

	h.nodes.set("n2", node.StateAvailable, "")
	h.tick(t)
	h.tick(t)

	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
}

func TestRetriedReplacementCanStillSucceedAfterAFailedAttempt(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateRacked, "")
	h.nodes.set("n3", node.StateAvailable, "")

	h.tick(t)
	h.store.replacements["n1"] = "n3"
	h.nodes.set("n2", node.StateAvailable, "")
	h.tick(t)

	assertEqual(t, "status", h.status("res-1"), StatusActive)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 0)
}

func TestDegradedNotCountedUntilTheStatusIsStored(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateAvailable, "")
	h.store.statusErrs = []error{errors.New("db down")}

	if err := h.rec.Tick(context.Background()); err == nil {
		t.Fatal("Tick = nil, want the status write error")
	}
	assertEqual(t, "degraded count after failed write", h.metrics.degradedCount(), 0)

	h.tick(t)

	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 1)
	assertEvents(t, h, provisioned("n2"))
}

func TestStartSkipsNodesThatAreAlreadyStarted(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2", "n3")
	h.nodes.hold("n1", node.StateProvisioning, "res-1")
	h.nodes.hold("n2", node.StateActive, "res-1")
	h.nodes.set("n3", node.StateAvailable, "")

	h.tick(t)

	assertEvents(t, h, provisioned("n3"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
}

func TestStartIsIdempotentAfterAFailedStatusWrite(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateAvailable, "")
	h.nodes.set("n2", node.StateAvailable, "")
	h.store.statusErrs = []error{errors.New("db down")}

	if err := h.rec.Tick(context.Background()); err == nil {
		t.Fatal("Tick = nil, want the status write error")
	}
	assertEqual(t, "status after failed write", h.status("res-1"), StatusScheduled)

	h.tick(t)
	h.tick(t)

	assertEvents(t, h, provisioned("n1"), provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
}

func TestStartRetriesWhenTheNodeChangedUnderneath(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1")
	h.nodes.set("n1", node.StateAvailable, "")
	h.nodes.failApply("n1", fmt.Errorf("apply: %w", node.ErrInvalidTransition))

	h.tick(t)
	assertEvents(t, h)
	assertEqual(t, "status after conflict", h.status("res-1"), StatusScheduled)
	if !strings.Contains(h.logs.String(), "retrying next tick") {
		t.Errorf("conflict not logged: %s", h.logs)
	}

	h.tick(t)
	assertEvents(t, h, provisioned("n1"))
	assertEqual(t, "status", h.status("res-1"), StatusActive)
}

func TestStartErrorsDoNotBlockOtherNodesOrReservations(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-bad", StatusScheduled, "missing", "n1")
	h.reserve("res-ok", StatusScheduled, "n2")
	h.nodes.set("n1", node.StateAvailable, "")
	h.nodes.set("n2", node.StateAvailable, "")

	err := h.rec.Tick(context.Background())

	if !errors.Is(err, errNoSuchNode) || !strings.Contains(err.Error(), "res-bad") {
		t.Fatalf("Tick = %v, want the missing node error naming res-bad", err)
	}
	assertEvents(t, h, provisioned("n1"), provisioned("n2"))
	assertEqual(t, "failing reservation", h.status("res-bad"), StatusScheduled)
	assertEqual(t, "healthy reservation", h.status("res-ok"), StatusActive)
}

func TestStartReplaceErrorKeepsReservationScheduled(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateAvailable, "")
	h.store.replaceErrs["n1"] = errors.New("db down")

	err := h.rec.Tick(context.Background())

	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("Tick = %v, want the replace error", err)
	}
	assertEvents(t, h, provisioned("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusScheduled)
	assertEqual(t, "degraded count", h.metrics.degradedCount(), 0)
}

func TestStartEndsAReservationWhoseWindowAlreadyPassed(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1")
	h.nodes.set("n1", node.StateAvailable, "")
	h.expire()

	h.tick(t)

	assertEvents(t, h)
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}

func TestStartFiresWhenTheClockReachesStartsAt(t *testing.T) {
	h := newHarness(t)
	h.store.add(Reservation{
		ID: "res-1", Tenant: testTenant, Status: StatusScheduled, NodeIDs: []string{"n1"},
		StartsAt: testStart.Add(testTick), EndsAt: testStart.Add(time.Hour),
	})
	h.nodes.set("n1", node.StateAvailable, "")

	h.tick(t)
	assertEqual(t, "status before start", h.status("res-1"), StatusScheduled)

	if err := h.clock.Sleep(context.Background(), testTick); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	h.tick(t)
	assertEqual(t, "status at start", h.status("res-1"), StatusActive)
}

func TestStartDoesNotCountANodeHeldByTheTenantsPreviousReservation(t *testing.T) {
	h := newHarness(t)
	h.store.add(Reservation{
		ID: "res-0", Tenant: testTenant, Count: 1, Policy: "any", Status: StatusActive, NodeIDs: []string{"n1"},
		StartsAt: testStart.Add(-time.Hour), EndsAt: testStart,
	})
	h.reserve("res-1", StatusScheduled, "n1")
	h.nodes.hold("n1", node.StateProvisioning, "res-0")

	h.tick(t)
	assertEqual(t, "status while res-0 still provisions n1", h.status("res-1"), StatusScheduled)

	h.nodes.hold("n1", node.StateActive, "res-0")
	h.tick(t)
	assertEvents(t, h, released("n1"))
	assertEqual(t, "previous reservation", h.status("res-0"), StatusEnded)
	assertEqual(t, "status while n1 wipes", h.status("res-1"), StatusScheduled)
}

func TestStartDropsUnreplaceableNodesOnlyOnceTheRestHaveStarted(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusScheduled, "n1", "n2")
	h.nodes.set("n1", node.StateQuarantined, "")
	h.nodes.set("n2", node.StateRacked, "")

	h.tick(t)
	assertEqual(t, "drops while waiting", len(h.store.drops), 0)

	h.nodes.set("n2", node.StateAvailable, "")
	h.tick(t)

	assertEqual(t, "dropped", strings.Join(h.store.drops, ","), "n1")
	assertEqual(t, "status", h.status("res-1"), StatusDegraded)
}

func TestStartWindsDownAReservationCancelledMidStart(t *testing.T) {
	h := newHarness(t)
	h.store.add(Reservation{
		ID: "res-1", Tenant: testTenant, Count: 3, Policy: "any", Status: StatusScheduled,
		NodeIDs:  []string{"n1", "n2", "n3"},
		StartsAt: testStart.Add(-time.Minute), EndsAt: testStart,
	})
	h.nodes.hold("n1", node.StateActive, "res-1")
	h.nodes.set("n2", node.StateAvailable, "")
	h.nodes.hold("n3", node.StateProvisioning, "res-1")

	h.tick(t)
	assertEvents(t, h, released("n1"))
	assertEqual(t, "status while n3 provisions", h.status("res-1"), StatusScheduled)

	h.nodes.hold("n3", node.StateActive, "res-1")
	h.tick(t)

	assertEvents(t, h, released("n1"), released("n3"))
	assertEqual(t, "unstarted node untouched", h.nodes.get("n2").State, node.StateAvailable)
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}
