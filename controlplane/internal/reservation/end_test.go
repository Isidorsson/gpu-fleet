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

func TestEndReleasesActiveNodesAndEnds(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1", "n2")
	h.nodes.set("n1", node.StateActive, testTenant)
	h.nodes.set("n2", node.StateActive, testTenant)
	h.expire()

	h.tick(t)

	assertEvents(t, h, released("n1"), released("n2"))
	assertEqual(t, "n1 state", h.nodes.get("n1").State, node.StateWiping)
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}

func TestEndIgnoresReservationsThatAreNotDue(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1")
	h.nodes.set("n1", node.StateActive, testTenant)

	h.tick(t)

	assertEvents(t, h)
	assertEqual(t, "status", h.status("res-1"), StatusActive)
}

func TestEndWaitsForProvisioningNodes(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1", "n2")
	h.nodes.set("n1", node.StateActive, testTenant)
	h.nodes.set("n2", node.StateProvisioning, testTenant)
	h.expire()

	h.tick(t)
	assertEvents(t, h, released("n1"))
	assertEqual(t, "status while n2 provisions", h.status("res-1"), StatusActive)

	h.tick(t)
	assertEvents(t, h, released("n1"))
	assertEqual(t, "status still waiting", h.status("res-1"), StatusActive)

	h.nodes.set("n2", node.StateActive, testTenant)
	h.tick(t)

	assertEvents(t, h, released("n1"), released("n2"))
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}

func TestEndLeavesOtherTenantsAndQuarantinedNodesAlone(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusDegraded, "n1", "n2", "n3")
	h.nodes.set("n1", node.StateActive, otherTenant)
	h.nodes.set("n2", node.StateQuarantined, testTenant)
	h.nodes.set("n3", node.StateActive, testTenant)
	h.expire()

	h.tick(t)

	assertEvents(t, h, released("n3"))
	assertEqual(t, "other tenant's node", h.nodes.get("n1").State, node.StateActive)
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}

func TestEndIsIdempotentAfterAFailedStatusWrite(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1")
	h.nodes.set("n1", node.StateActive, testTenant)
	h.expire()
	h.store.statusErrs = []error{errors.New("db down")}

	if err := h.rec.Tick(context.Background()); err == nil {
		t.Fatal("Tick = nil, want the status write error")
	}
	h.tick(t)
	h.tick(t)

	assertEvents(t, h, released("n1"))
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}

func TestEndRetriesWhenTheNodeChangedUnderneath(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1")
	h.nodes.set("n1", node.StateActive, testTenant)
	h.nodes.failApply("n1", fmt.Errorf("apply: %w", node.ErrInvalidTransition))
	h.expire()

	h.tick(t)
	assertEvents(t, h)
	assertEqual(t, "status after conflict", h.status("res-1"), StatusActive)
	if !strings.Contains(h.logs.String(), "retrying next tick") {
		t.Errorf("conflict not logged: %s", h.logs)
	}

	h.tick(t)
	assertEvents(t, h, released("n1"))
	assertEqual(t, "status", h.status("res-1"), StatusEnded)
}

func TestEndErrorsDoNotBlockOtherReservations(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-bad", StatusActive, "missing")
	h.reserve("res-ok", StatusActive, "n1")
	h.nodes.set("n1", node.StateActive, testTenant)
	h.expire()

	err := h.rec.Tick(context.Background())

	if !errors.Is(err, errNoSuchNode) {
		t.Fatalf("Tick = %v, want the missing node error", err)
	}
	assertEqual(t, "failing reservation", h.status("res-bad"), StatusActive)
	assertEqual(t, "healthy reservation", h.status("res-ok"), StatusEnded)
}

// A tenant's back-to-back reservations may share a node: the second must see
// it wiping, not still running for the first.
func TestEndRunsBeforeStartSoBackToBackReservationsDoNotShareARunningNode(t *testing.T) {
	h := newHarness(t)
	h.reserve("res-1", StatusActive, "n1")
	h.store.add(Reservation{
		ID: "res-2", Tenant: testTenant, Status: StatusScheduled, NodeIDs: []string{"n1"},
		StartsAt: testStart.Add(time.Hour), EndsAt: testStart.Add(2 * time.Hour),
	})
	h.nodes.set("n1", node.StateActive, testTenant)
	h.clock.now = testStart.Add(time.Hour)

	h.tick(t)

	assertEvents(t, h, released("n1"))
	assertEqual(t, "first reservation", h.status("res-1"), StatusEnded)
	assertEqual(t, "second reservation", h.status("res-2"), StatusScheduled)
}
