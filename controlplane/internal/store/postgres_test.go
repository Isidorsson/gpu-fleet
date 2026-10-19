package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"gpu-fleet/controlplane/internal/node"
)

// Integration tests run against TEST_DATABASE_URL and are skipped without it.
// Each test uses node IDs derived from its own name, so tests share one
// migrated database without truncating between runs.
func openTestStore(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	p, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return p
}

func uniqueID(t *testing.T, label string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return strings.ReplaceAll(t.Name(), "/", "-") + "-" + label + "-" + hex.EncodeToString(b)
}

func registerAvailable(t *testing.T, p *Postgres, id string) {
	t.Helper()
	ctx := context.Background()
	if err := p.Register(ctx, node.Register(id, "r1")); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, ev := range []node.Event{node.EventStartInspection, node.EventInspectionPassed} {
		if _, err := p.ApplyEvent(ctx, id, ev, ""); err != nil {
			t.Fatalf("apply %s: %v", ev, err)
		}
	}
}

func TestRegisterAndGet(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	id := uniqueID(t, "n")

	if err := p.Register(ctx, node.Register(id, "r7")); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := p.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := node.Node{ID: id, Rack: "r7", State: node.StateRacked}
	if got != want {
		t.Fatalf("get = %+v, want %+v", got, want)
	}
}

func TestRegisterDuplicateConflicts(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	id := uniqueID(t, "n")
	if err := p.Register(ctx, node.Register(id, "r1")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := p.Register(ctx, node.Register(id, "r1")); !errors.Is(err, ErrConflict) {
		t.Fatalf("second register: err = %v, want ErrConflict", err)
	}
}

func TestGetMissingNode(t *testing.T) {
	p := openTestStore(t)
	if _, err := p.Get(context.Background(), uniqueID(t, "missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestApplyEventPersistsAndAudits(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	id := uniqueID(t, "n")
	registerAvailable(t, p, id)

	for _, step := range []struct {
		ev     node.Event
		tenant string
	}{
		{node.EventProvision, "acme"},
		{node.EventProvisionSucceeded, ""},
		{node.EventRelease, ""},
		{node.EventWipeSucceeded, ""},
	} {
		if _, err := p.ApplyEvent(ctx, id, step.ev, step.tenant); err != nil {
			t.Fatalf("apply %s: %v", step.ev, err)
		}
	}

	got, err := p.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != node.StateInspecting || got.Tenant != "" {
		t.Fatalf("node = %+v, want inspecting with no tenant", got)
	}

	events, err := p.Events(ctx, id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("got %d events, want 6", len(events))
	}
	wipe := events[5]
	if wipe.Event != node.EventWipeSucceeded || wipe.From != node.StateWiping || wipe.Tenant != "acme" {
		t.Fatalf("wipe event = %+v, want wiping->inspecting recorded for acme", wipe)
	}
}

func TestApplyEventInvalidTransitionChangesNothing(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	id := uniqueID(t, "n")
	if err := p.Register(ctx, node.Register(id, "r1")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := p.ApplyEvent(ctx, id, node.EventRelease, ""); !errors.Is(err, node.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	events, err := p.Events(ctx, id)
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %v, %v; want none", events, err)
	}
}

func TestApplyEventMissingNode(t *testing.T) {
	p := openTestStore(t)
	_, err := p.ApplyEvent(context.Background(), uniqueID(t, "missing"), node.EventStartInspection, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestConcurrentProvisionOnlyOneWins(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	id := uniqueID(t, "n")
	registerAvailable(t, p, id)

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			_, errs[i] = p.ApplyEvent(ctx, id, node.EventProvision, fmt.Sprintf("tenant-%d", i))
		})
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, node.ErrInvalidTransition):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d provisions succeeded, want exactly 1", wins)
	}
}

func TestListFiltersByState(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	racked, available := uniqueID(t, "racked"), uniqueID(t, "available")
	if err := p.Register(ctx, node.Register(racked, "r1")); err != nil {
		t.Fatalf("register: %v", err)
	}
	registerAvailable(t, p, available)

	nodes, err := p.List(ctx, node.StateAvailable)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, n := range nodes {
		if n.State != node.StateAvailable {
			t.Errorf("List(available) returned %+v", n)
		}
		ids[n.ID] = true
	}
	if !ids[available] || ids[racked] {
		t.Fatalf("List(available) ids = %v, want %s and not %s", ids, available, racked)
	}
}
