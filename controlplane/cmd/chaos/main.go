package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gpu-fleet/controlplane/internal/chaos"
	"gpu-fleet/controlplane/internal/logging"
)

func main() {
	if err := run(); err != nil {
		slog.Error("chaos exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := chaos.LoadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := logging.Setup()
	log.Info("chaos starting",
		"controlplane", cfg.ControlPlaneURL, "bmcsim", cfg.BMCSimURL, "node_bmc", cfg.NodeBMCURL,
		"agents", len(cfg.Agents), "seed", cfg.Seed, "tick", cfg.Tick)
	if err := chaos.New(cfg, log).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("chaos stopped")
	return nil
}
