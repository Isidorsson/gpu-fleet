package metrics

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gpu-fleet/controlplane/internal/node"
)

const fleetNodesHeader = "# HELP fleet_nodes Nodes by lifecycle state, counted at scrape time.\n# TYPE fleet_nodes gauge\n"

type fakeCounter struct {
	mu       sync.Mutex
	counts   map[node.State]int
	err      error
	block    bool
	calls    int
	deadline time.Time
}

func (f *fakeCounter) CountByState(ctx context.Context) (map[node.State]int, error) {
	f.mu.Lock()
	f.calls++
	f.deadline, _ = ctx.Deadline()
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.counts, f.err
}

func TestFleetNodesEmitsAllStatesWithZeros(t *testing.T) {
	c := newFleetCollector(&fakeCounter{counts: map[node.State]int{
		node.StateRacked: 2, node.StateQuarantined: 1,
	}})
	want := fleetNodesHeader +
		`fleet_nodes{state="racked"} 2
fleet_nodes{state="inspecting"} 0
fleet_nodes{state="available"} 0
fleet_nodes{state="provisioning"} 0
fleet_nodes{state="active"} 0
fleet_nodes{state="wiping"} 0
fleet_nodes{state="quarantined"} 1
`

	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "fleet_nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestFleetNodesEmptyFleetIsAllZeros(t *testing.T) {
	c := newFleetCollector(&fakeCounter{})
	want := fleetNodesHeader
	for _, st := range node.States() {
		want += `fleet_nodes{state="` + string(st) + "\"} 0\n"
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "fleet_nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestFleetNodesQueryRunsOncePerScrapeWithDeadline(t *testing.T) {
	counter := &fakeCounter{}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(newFleetCollector(counter)); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := reg.Gather(); err != nil {
			t.Fatal(err)
		}
	}

	if counter.calls != 3 {
		t.Errorf("queries = %d, want 3, one per scrape", counter.calls)
	}
	if counter.deadline.IsZero() {
		t.Error("query context has no deadline")
	}
}

func TestFleetNodesQueryErrorBecomesScrapeErrorWithoutPanic(t *testing.T) {
	boom := errors.New("db down")
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(newFleetCollector(&fakeCounter{err: boom})); err != nil {
		t.Fatal(err)
	}

	mfs, err := reg.Gather()

	if err == nil || !strings.Contains(err.Error(), "count nodes by state: db down") {
		t.Fatalf("Gather error = %v, want the wrapped query error", err)
	}
	if len(mfs) != 0 {
		t.Errorf("%d families reported after a failed query, want none", len(mfs))
	}
}

func TestFleetNodesQueryIsBoundedByTimeout(t *testing.T) {
	c := newFleetCollector(&fakeCounter{block: true})
	c.timeout = 20 * time.Millisecond
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := reg.Gather()

	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Gather error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Gather took %v, the timeout did not bound it", elapsed)
	}
}

func TestDefaultFleetTimeoutIsTheNamedConstant(t *testing.T) {
	if got := newFleetCollector(&fakeCounter{}).timeout; got != fleetQueryTimeout {
		t.Fatalf("timeout = %v, want %v", got, fleetQueryTimeout)
	}
}
