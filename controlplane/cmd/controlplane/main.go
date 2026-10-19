package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gpu-fleet/controlplane/internal/api"
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

	addr := os.Getenv("CONTROLPLANE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
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

	log := slog.Default()
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
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(apiHandler, m.Handler()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("controlplane listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		stop()
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
