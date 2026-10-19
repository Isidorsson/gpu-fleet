package provisioner

import (
	"context"
	"time"
)

// Clock lets tests run the worker without sleeping in real time.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d and returns ctx.Err() if ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
