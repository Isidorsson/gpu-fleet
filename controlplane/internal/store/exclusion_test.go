package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/reservation"
)

func TestExclusionConstraintRejectsOverlap(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n1", node.StateAvailable)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(0), at(2*time.Hour), "n1")

	for name, window := range map[string][2]time.Time{
		"starts inside": {at(time.Hour), at(3 * time.Hour)},
		"ends inside":   {at(-time.Hour), at(time.Hour)},
		"contains":      {at(-time.Hour), at(3 * time.Hour)},
		"contained":     {at(30 * time.Minute), at(time.Hour)},
		"identical":     {at(0), at(2 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			err := insertReservationRows(context.Background(), p, "rival-"+name,
				reservation.StatusScheduled, window[0], window[1], "n1")
			pgErr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pgErr.Code != pgExclusionViolation {
				t.Fatalf("overlapping insert err = %v, want SQLSTATE %s", err, pgExclusionViolation)
			}
		})
	}
}

func TestExclusionConstraintAllowsBackToBackAndOtherNodes(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n1", node.StateAvailable)
	registerIn(t, p, "n2", node.StateAvailable)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(0), at(time.Hour), "n1")

	mustInsertReservation(t, p, "r2", reservation.StatusScheduled, at(time.Hour), at(2*time.Hour), "n1")
	mustInsertReservation(t, p, "r0", reservation.StatusScheduled, at(-time.Hour), at(0), "n1")
	mustInsertReservation(t, p, "r3", reservation.StatusScheduled, at(0), at(time.Hour), "n2")
}

func TestReservationConstraints(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n1", node.StateAvailable)
	registerIn(t, p, "n2", node.StateAvailable)
	mustInsertReservation(t, p, "x", reservation.StatusScheduled, at(0), at(time.Hour), "n2")
	ctx := context.Background()

	tests := map[string]string{
		"empty period":    `INSERT INTO reservations (id, tenant, node_count, policy, period, status) VALUES ('y', 't', 1, 'any', 'empty', 'scheduled')`,
		"zero nodes":      `INSERT INTO reservations (id, tenant, node_count, policy, period, status) VALUES ('y', 't', 0, 'any', tstzrange(now(), now() + interval '1 hour'), 'scheduled')`,
		"65 nodes":        `INSERT INTO reservations (id, tenant, node_count, policy, period, status) VALUES ('y', 't', 65, 'any', tstzrange(now(), now() + interval '1 hour'), 'scheduled')`,
		"unknown policy":  `INSERT INTO reservations (id, tenant, node_count, policy, period, status) VALUES ('y', 't', 1, 'nearby', tstzrange(now(), now() + interval '1 hour'), 'scheduled')`,
		"unknown status":  `INSERT INTO reservations (id, tenant, node_count, policy, period, status) VALUES ('y', 't', 1, 'any', tstzrange(now(), now() + interval '1 hour'), 'paused')`,
		"unknown node":    `INSERT INTO reservation_nodes (reservation_id, node_id, period) VALUES ('x', 'ghost', tstzrange(now(), now() + interval '1 hour'))`,
		"empty node span": `INSERT INTO reservation_nodes (reservation_id, node_id, period) VALUES ('x', 'n1', 'empty')`,
	}
	for name, sql := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := p.pool.Exec(ctx, sql); err == nil {
				t.Fatal("insert succeeded, want constraint violation")
			}
		})
	}
}

func TestDeletingReservationCascadesToNodes(t *testing.T) {
	p := openReservationStore(t)
	registerIn(t, p, "n1", node.StateAvailable)
	mustInsertReservation(t, p, "r1", reservation.StatusScheduled, at(0), at(time.Hour), "n1")

	if _, err := p.pool.Exec(context.Background(), `DELETE FROM reservations WHERE id = 'r1'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := countRows(t, p, "reservation_nodes"); n != 0 {
		t.Fatalf("%d node rows left, want 0", n)
	}
}
