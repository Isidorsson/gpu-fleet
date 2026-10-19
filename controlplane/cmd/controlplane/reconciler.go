package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
	"gpu-fleet/controlplane/internal/store"
)

const leaderCloseTimeout = 5 * time.Second

// startReconciler runs the reservation reconciler until ctx ends. Every
// replica runs one; the advisory-lock leader makes only one of them act.
func startReconciler(ctx context.Context, db *store.Postgres, m reservation.Metrics, log *slog.Logger) (*sync.WaitGroup, error) {
	leader := db.NewLeader(reservation.DefaultLockKey)
	rec, err := reservation.NewReconciler(
		reservation.Config{Tick: reservation.DefaultTick, LockKey: reservation.DefaultLockKey},
		reservation.Deps{Store: db, Nodes: db, Leader: leader, Place: placement.Place, Metrics: m, Log: log},
	)
	if err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := rec.Run(ctx); err != nil {
			log.Error("reconciler stopped", "err", err)
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaderCloseTimeout)
		defer cancel()
		if err := leader.Close(closeCtx); err != nil {
			log.Error("release reconciler leadership", "err", err)
		}
	})
	return &wg, nil
}
