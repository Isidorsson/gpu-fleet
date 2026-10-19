package chaos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetryRetriesTransientFailuresThenSucceeds(t *testing.T) {
	attempts := 0
	err := fastBackoff.Retry(context.Background(), "thing", func(context.Context) error {
		attempts++
		if attempts < 4 {
			return fmt.Errorf("%w: refused", errTransport)
		}
		return nil
	})
	if err != nil || attempts != 4 {
		t.Errorf("err %v after %d attempts, want success after 4", err, attempts)
	}
}

func TestRetryTreatsServerErrorsAsTransientAndClientErrorsAsFinal(t *testing.T) {
	tests := []struct {
		status    int
		transient bool
	}{
		{500, true}, {502, true}, {503, true}, {504, true}, {429, true}, {408, true},
		{400, false}, {401, false}, {403, false}, {404, false}, {409, false}, {422, false},
	}
	for _, tt := range tests {
		attempts := 0
		err := fastBackoff.Retry(context.Background(), "thing", func(context.Context) error {
			attempts++
			if attempts == 2 {
				return nil
			}
			return &StatusError{Method: "GET", Path: "/x", Status: tt.status}
		})
		if tt.transient && (err != nil || attempts != 2) {
			t.Errorf("status %d: err %v after %d attempts, want a retry", tt.status, err, attempts)
		}
		if !tt.transient && (err == nil || attempts != 1) {
			t.Errorf("status %d: err %v after %d attempts, want an immediate failure", tt.status, err, attempts)
		}
	}
}

func TestRetryDoesNotRetryUnknownErrors(t *testing.T) {
	attempts := 0
	err := fastBackoff.Retry(context.Background(), "thing", func(context.Context) error {
		attempts++
		return errors.New("decode failed")
	})
	if err == nil || attempts != 1 {
		t.Errorf("err %v after %d attempts", err, attempts)
	}
}

func TestRetryGivesUpAtTheTimeoutAndWrapsTheLastError(t *testing.T) {
	cause := fmt.Errorf("%w: refused", errTransport)
	b := Backoff{Initial: time.Millisecond, Max: 4 * time.Millisecond, Timeout: 40 * time.Millisecond}
	start := time.Now()
	err := b.Retry(context.Background(), "control plane", func(context.Context) error { return cause })
	if !errors.Is(err, errTransport) || !strings.Contains(err.Error(), "control plane: gave up after") {
		t.Errorf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s for a 40ms timeout", elapsed)
	}
}

func TestRetryDelayDoublesUpToTheMax(t *testing.T) {
	var times []time.Time
	b := Backoff{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond, Timeout: time.Minute}
	_ = b.Retry(context.Background(), "thing", func(context.Context) error {
		times = append(times, time.Now())
		if len(times) == 5 {
			return nil
		}
		return fmt.Errorf("%w: refused", errTransport)
	})
	if len(times) != 5 {
		t.Fatalf("%d attempts", len(times))
	}
	gaps := []time.Duration{10, 20, 20, 20}
	for i, want := range gaps {
		got := times[i+1].Sub(times[i])
		if got < want*time.Millisecond {
			t.Errorf("gap %d = %s, want at least %s", i, got, want*time.Millisecond)
		}
	}
	if total := times[4].Sub(times[0]); total >= 500*time.Millisecond {
		t.Errorf("total wait %s; the delay must be capped at Max", total)
	}
}

func TestRetryStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := fastBackoff.Retry(ctx, "thing", func(context.Context) error {
		attempts++
		if attempts == 2 {
			cancel()
		}
		return fmt.Errorf("%w: refused", errTransport)
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}
