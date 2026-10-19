package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gpu-fleet/controlplane/internal/node"
)

// Integration tests run against TEST_DATABASE_URL and are skipped without it.
// Each test gets its own schema, created before and dropped after, so tests
// are isolated even for queries that look at every node (ClaimWork).
func openTestStore(t *testing.T) *Postgres {
	t.Helper()
	return openTestStoreWithConns(t, 0)
}

// openTestStoreWithConns is openTestStore with a pool of maxConns connections,
// or the driver default when it is 0, for tests that hold many open at once.
func openTestStoreWithConns(t *testing.T, maxConns int32) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = createTestSchema(t, url)
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	p, err := openPool(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return p
}

func createTestSchema(t *testing.T, url string) string {
	t.Helper()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "test_" + hex.EncodeToString(suffix)
	quoted := pgx.Identifier{schema}.Sanitize()
	execAdmin(t, url, "CREATE SCHEMA "+quoted)
	t.Cleanup(func() { execAdmin(t, url, "DROP SCHEMA "+quoted+" CASCADE") })
	return schema
}

func execAdmin(t *testing.T, url, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func testBMC(id string) string { return "http://" + id + ".bmc.test:8443" }

func register(t *testing.T, p *Postgres, id string) { registerAt(t, p, id, "p1", "r1") }

func registerAt(t *testing.T, p *Postgres, id, pod, rack string) {
	t.Helper()
	if err := p.Register(context.Background(), node.Register(node.Spec{ID: id, Pod: pod, Rack: rack, BMC: testBMC(id)})); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// pathTo lists the events that take a freshly racked node to each state.
var pathTo = map[node.State][]node.Event{
	node.StateRacked:       nil,
	node.StateInspecting:   {node.EventStartInspection},
	node.StateAvailable:    {node.EventStartInspection, node.EventInspectionPassed},
	node.StateProvisioning: {node.EventStartInspection, node.EventInspectionPassed, node.EventProvision},
	node.StateActive: {
		node.EventStartInspection, node.EventInspectionPassed, node.EventProvision,
		node.EventProvisionSucceeded,
	},
	node.StateWiping: {
		node.EventStartInspection, node.EventInspectionPassed, node.EventProvision,
		node.EventProvisionSucceeded, node.EventRelease,
	},
	node.StateQuarantined: {node.EventStartInspection, node.EventInspectionFailed},
}

func registerIn(t *testing.T, p *Postgres, id string, state node.State) {
	t.Helper()
	registerAtIn(t, p, id, "p1", "r1", state)
}

func registerAtIn(t *testing.T, p *Postgres, id, pod, rack string, state node.State) {
	t.Helper()
	registerAt(t, p, id, pod, rack)
	for _, ev := range pathTo[state] {
		tenant := ""
		if ev == node.EventProvision {
			tenant = "acme"
		}
		if _, err := p.ApplyEvent(context.Background(), id, ev, tenant); err != nil {
			t.Fatalf("apply %s to %s: %v", ev, id, err)
		}
	}
}

func TestRegisterAndGet(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()

	want := node.Node{ID: "n1", Pod: "p3", Rack: "r7", BMC: "https://10.0.0.7:8443", State: node.StateRacked}
	if err := p.Register(ctx, node.Register(node.Spec{ID: want.ID, Pod: want.Pod, Rack: want.Rack, BMC: want.BMC})); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := p.Get(ctx, "n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != want {
		t.Fatalf("get = %+v, want %+v", got, want)
	}
}

func TestBMCRoundTripsThroughList(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "a", node.StateRacked)
	registerIn(t, p, "b", node.StateAvailable)

	nodes, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("list returned %d nodes, want 2", len(nodes))
	}
	for _, n := range nodes {
		if n.BMC != testBMC(n.ID) {
			t.Errorf("node %s bmc = %q, want %q", n.ID, n.BMC, testBMC(n.ID))
		}
	}
}

func TestPodRoundTripsThroughListClaimAndTransition(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerAt(t, p, "a", "pod-7", "r1")
	registerAt(t, p, "b", "pod-8", "r2")

	nodes, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Pod != "pod-7" || nodes[1].Pod != "pod-8" {
		t.Fatalf("list pods = %+v, want pod-7 then pod-8", nodes)
	}

	claimed, err := p.ClaimWork(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Pod != "pod-7" || claimed.Rack != "r1" {
		t.Fatalf("claimed %+v, want pod-7/r1", claimed)
	}

	moved, err := p.ApplyEvent(ctx, "b", node.EventStartInspection, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if moved.Pod != "pod-8" || moved.Rack != "r2" {
		t.Fatalf("transitioned %+v, want pod-8/r2", moved)
	}
}

func TestRegisterDuplicateConflicts(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")
	err := p.Register(context.Background(), node.Register(node.Spec{ID: "n1", Pod: "p1", Rack: "r1", BMC: testBMC("n1")}))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second register: err = %v, want ErrConflict", err)
	}
}

func TestGetMissingNode(t *testing.T) {
	p := openTestStore(t)
	if _, err := p.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestApplyEventPersistsAndAudits(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "n1", node.StateAvailable)

	for _, step := range []struct {
		ev     node.Event
		tenant string
	}{
		{node.EventProvision, "acme"},
		{node.EventProvisionSucceeded, ""},
		{node.EventRelease, ""},
		{node.EventWipeSucceeded, ""},
	} {
		if _, err := p.ApplyEvent(ctx, "n1", step.ev, step.tenant); err != nil {
			t.Fatalf("apply %s: %v", step.ev, err)
		}
	}

	got, err := p.Get(ctx, "n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != node.StateInspecting || got.Tenant != "" {
		t.Fatalf("node = %+v, want inspecting with no tenant", got)
	}

	events, err := p.Events(ctx, "n1")
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
	register(t, p, "n1")
	if _, err := p.ApplyEvent(ctx, "n1", node.EventRelease, ""); !errors.Is(err, node.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	events, err := p.Events(ctx, "n1")
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %v, %v; want none", events, err)
	}
}

func TestApplyEventMissingNode(t *testing.T) {
	p := openTestStore(t)
	_, err := p.ApplyEvent(context.Background(), "missing", node.EventStartInspection, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestConcurrentProvisionOnlyOneWins(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()
	registerIn(t, p, "n1", node.StateAvailable)

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			_, errs[i] = p.ApplyEvent(ctx, "n1", node.EventProvision, fmt.Sprintf("tenant-%d", i))
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
	registerIn(t, p, "racked", node.StateRacked)
	registerIn(t, p, "available", node.StateAvailable)

	nodes, err := p.List(context.Background(), node.StateAvailable)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "available" {
		t.Fatalf("List(available) = %+v, want only the available node", nodes)
	}
}

func TestCountByState(t *testing.T) {
	p := openTestStore(t)
	ctx := context.Background()

	empty, err := p.CountByState(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("CountByState on empty table = %v, %v; want empty map", empty, err)
	}

	registerIn(t, p, "r1", node.StateRacked)
	registerIn(t, p, "r2", node.StateRacked)
	registerIn(t, p, "a1", node.StateAvailable)
	registerIn(t, p, "q1", node.StateQuarantined)
	registerIn(t, p, "w1", node.StateWiping)

	got, err := p.CountByState(ctx)
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	want := map[node.State]int{
		node.StateRacked: 2, node.StateAvailable: 1, node.StateQuarantined: 1, node.StateWiping: 1,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("CountByState = %v, want %v", got, want)
	}
}

func TestLeaseColumnsMustBeSetTogether(t *testing.T) {
	p := openTestStore(t)
	register(t, p, "n1")
	_, err := p.pool.Exec(context.Background(), `UPDATE nodes SET lease_owner = 'w1' WHERE id = 'n1'`)
	if err == nil {
		t.Fatal("setting lease_owner without lease_expires_at succeeded, want check violation")
	}
}
