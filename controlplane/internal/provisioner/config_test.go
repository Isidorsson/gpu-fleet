package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no owner", func(c *Config) { c.Owner = "" }},
		{"zero lease ttl", func(c *Config) { c.LeaseTTL = 0 }},
		{"negative lease ttl", func(c *Config) { c.LeaseTTL = -time.Second }},
		{"zero poll interval", func(c *Config) { c.PollInterval = 0 }},
		{"poll interval above half the lease", func(c *Config) { c.PollInterval = c.LeaseTTL/2 + 1 }},
		{"zero idle backoff", func(c *Config) { c.IdleBackoff = 0 }},
		{"zero task timeout", func(c *Config) { c.TaskTimeout = 0 }},
		{"zero attempts", func(c *Config) { c.MaxAttempts = 0 }},
		{"negative attempts", func(c *Config) { c.MaxAttempts = -1 }},
		{"zero retry backoff", func(c *Config) { c.RetryBackoff = 0 }},
		{"retry backoff above half the lease", func(c *Config) { c.RetryBackoff = c.LeaseTTL/2 + 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mutate(&cfg)

			_, err := NewWorker(cfg, validDeps())

			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewWorker error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigValidationReportsEveryProblem(t *testing.T) {
	_, err := NewWorker(Config{}, validDeps())

	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
	for _, field := range []string{"Owner", "LeaseTTL", "PollInterval", "IdleBackoff", "TaskTimeout", "MaxAttempts", "RetryBackoff"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not mention %s: %v", field, err)
		}
	}
}

func TestMinimalValidConfig(t *testing.T) {
	cfg := Config{
		Owner:        "w",
		LeaseTTL:     2 * time.Nanosecond,
		PollInterval: time.Nanosecond,
		IdleBackoff:  time.Nanosecond,
		TaskTimeout:  time.Nanosecond,
		MaxAttempts:  1,
		RetryBackoff: time.Nanosecond,
	}

	if _, err := NewWorker(cfg, validDeps()); err != nil {
		t.Fatalf("NewWorker = %v, want nil", err)
	}
}

func TestMissingDependenciesAreRejected(t *testing.T) {
	good := validDeps()
	tests := []struct {
		name string
		deps Deps
	}{
		{"no store", Deps{Dial: good.Dial}},
		{"no dial", Deps{Store: good.Store}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWorker(testConfig(), tc.deps)

			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewWorker error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestDefaultsClockAndLogger(t *testing.T) {
	w, err := NewWorker(testConfig(), validDeps())
	if err != nil {
		t.Fatal(err)
	}

	if w.clock == nil || w.log == nil {
		t.Fatalf("clock = %v, log = %v, want both defaulted", w.clock, w.log)
	}
	worked, err := w.Step(context.Background())
	if worked || err != nil {
		t.Fatalf("Step = (%v, %v), want (false, nil)", worked, err)
	}
}

func TestWallClockSleep(t *testing.T) {
	var c Clock = wallClock{}

	if err := c.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep on canceled ctx = %v, want context.Canceled", err)
	}
	if c.Now().IsZero() {
		t.Fatal("Now is zero")
	}
}

func validDeps() Deps {
	return Deps{Store: newFakeStore(), Dial: newFakeMachine().dial}
}
