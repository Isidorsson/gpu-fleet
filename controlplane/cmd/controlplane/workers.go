package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/provisioner"
	"gpu-fleet/controlplane/internal/redfish"
	"gpu-fleet/controlplane/internal/store"
)

const defaultWorkers = 4

var workerConfig = provisioner.Config{
	LeaseTTL:     30 * time.Second,
	PollInterval: time.Second,
	IdleBackoff:  2 * time.Second,
	TaskTimeout:  10 * time.Minute,
	MaxAttempts:  5,
	RetryBackoff: 500 * time.Millisecond,
}

// startWorkers runs PROVISIONER_WORKERS provisioner workers until ctx ends.
// Each gets a unique owner so leases from this process and its replicas never
// collide. BMC credentials come from BMC_USER and BMC_PASSWORD.
func startWorkers(ctx context.Context, db *store.Postgres, log *slog.Logger) (*sync.WaitGroup, error) {
	count, err := workerCount()
	if err != nil {
		return nil, err
	}
	user, password := os.Getenv("BMC_USER"), os.Getenv("BMC_PASSWORD")
	if count > 0 && (user == "" || password == "") {
		return nil, errors.New("BMC_USER and BMC_PASSWORD are required when workers are enabled")
	}
	dial := func(url string) (provisioner.BMC, provisioner.Agent) {
		return redfish.New(url, user, password, nil), deployagent.New(url, user, password, nil)
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}

	var wg sync.WaitGroup
	for i := range count {
		cfg := workerConfig
		cfg.Owner = fmt.Sprintf("%s/%d/%d", host, os.Getpid(), i)
		w, err := provisioner.NewWorker(cfg, provisioner.Deps{Store: db, Dial: dial, Log: log})
		if err != nil {
			return nil, err
		}
		wg.Go(func() {
			if err := w.Run(ctx); err != nil {
				log.Error("worker stopped", "owner", cfg.Owner, "err", err)
			}
		})
	}
	log.Info("provisioner workers started", "count", count)
	return &wg, nil
}

func workerCount() (int, error) {
	raw := os.Getenv("PROVISIONER_WORKERS")
	if raw == "" {
		return defaultWorkers, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("PROVISIONER_WORKERS must be a non-negative integer, got %q", raw)
	}
	return n, nil
}
