package chaos

import (
	"context"
	"fmt"
	"time"
)

const (
	startupInitialDelay = 500 * time.Millisecond
	startupMaxDelay     = 5 * time.Second
	startupTimeout      = 5 * time.Minute
)

// Backoff bounds how long startup waits for a dependency: the delay doubles
// from Initial up to Max, and Retry gives up once Timeout would be exceeded.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Timeout time.Duration
}

var defaultBackoff = Backoff{Initial: startupInitialDelay, Max: startupMaxDelay, Timeout: startupTimeout}

// Retry runs fn until it succeeds. Only transient failures (no answer, 5xx,
// 429, 408) are retried; anything else, like bad credentials, fails at once.
func (b Backoff) Retry(ctx context.Context, what string, fn func(context.Context) error) error {
	deadline := time.Now().Add(b.Timeout)
	delay := b.Initial
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		if !transient(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if !time.Now().Add(delay).Before(deadline) {
			return fmt.Errorf("%s: gave up after %d attempts: %w", what, attempt, err)
		}
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		delay = min(delay*2, b.Max)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
