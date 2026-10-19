package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gpu-fleet/controlplane/internal/api"
	"gpu-fleet/controlplane/internal/logging"
	"gpu-fleet/controlplane/internal/metrics"
	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/store"
)

const (
	defaultAddr     = ":8080"
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("controlplane exited", "err", err)
		os.Exit(1)
	}
}

// newHandler serves GET /metrics next to the API on one listener.
func newHandler(apiHandler, metricsHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler)
	mux.Handle("/", apiHandler)
	return mux
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}

	log := logging.Setup()
	m, err := metrics.New(db, log)
	if err != nil {
		return err
	}
	if err := m.RegisterCapacity(db); err != nil {
		return err
	}
	creds := bmcCredentialsFromEnv()
	attestations, err := newAttestation(db, creds, log)
	if err != nil {
		return err
	}
	workers, err := startWorkers(ctx, workerDeps{
		db: db, metrics: m.Provisioner(), attester: attestations, creds: creds, log: log,
	})
	if err != nil {
		return err
	}
	defer workers.Wait()
	reconciler, err := startReconciler(ctx, db, m.Reservations(), log)
	if err != nil {
		return err
	}
	defer reconciler.Wait()
	remediator, err := newRemediation(db, m.Remediation(), log)
	if err != nil {
		return err
	}

	apiHandler := api.NewHandler(api.Deps{
		Store:          db,
		Place:          placement.Place,
		Summarize:      placement.Summarize,
		Metrics:        m.Reservations(),
		Attestations:   attestations,
		Remediator:     remediator.service,
		RemediationLog: db,
		WebhookToken:   remediator.token,
		Log:            log,
	})
	err = serve(ctx, listeners(newHandler(apiHandler, m.Handler())), log)
	// A failed listener must also stop the workers and reconciler, or the
	// deferred Waits block forever.
	stop()
	return err
}
