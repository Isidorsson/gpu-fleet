package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gpu-fleet/controlplane/internal/bmcsim"
)

const (
	defaultAddr       = ":8443"
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	userEnv           = "BMCSIM_USER"
	passwordEnv       = "BMCSIM_PASSWORD"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("bmcsim exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr, cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		return err
	}
	handler, err := bmcsim.NewHandler(cfg, bmcsim.SystemClock{})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := slog.Default()
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("bmcsim listening", "addr", addr, "nodes", cfg.Nodes, "seed", cfg.Seed)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
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

func parseConfig(args []string) (string, bmcsim.Config, error) {
	var (
		addr string
		cfg  bmcsim.Config
	)
	fs := flag.NewFlagSet("bmcsim", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultAddr, "listen address")
	fs.IntVar(&cfg.Nodes, "nodes", 50, "number of simulated machines")
	fs.Uint64Var(&cfg.Seed, "seed", 42, "seed for reproducible outcomes")
	fs.DurationVar(&cfg.BootLatency, "boot-latency", 3*time.Second, "time from power-on until the agent starts its task")
	fs.DurationVar(&cfg.TaskDuration, "task-duration", 5*time.Second, "time a task takes once the agent has started")
	fs.Float64Var(&cfg.InspectFail, "inspect-fail", 0.05, "probability an inspect task fails")
	fs.Float64Var(&cfg.WipeFail, "wipe-fail", 0.02, "probability a wipe task fails")
	fs.Float64Var(&cfg.DeployFail, "deploy-fail", 0.02, "probability a deploy task fails")
	fs.Float64Var(&cfg.BMCErrorRate, "bmc-error-rate", 0.01, "probability an authenticated request returns 503")
	if err := fs.Parse(args); err != nil {
		return "", cfg, err
	}

	cfg.User = os.Getenv(userEnv)
	cfg.Password = os.Getenv(passwordEnv)
	if cfg.User == "" || cfg.Password == "" {
		return "", cfg, fmt.Errorf("%s and %s are required", userEnv, passwordEnv)
	}
	return addr, cfg, nil
}
