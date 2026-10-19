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

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

const (
	freeNodesHelp    = "Nodes free for the next minute, by pod, counted at scrape time."
	reservationsHelp = "Reservations by status, counted at scrape time."
	failuresHelp     = "Requests rejected for insufficient capacity, by policy."
	degradedHelp     = "Reservations that became degraded, at start or while running."
)

var testNow = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

type fakeCapacity struct {
	mu       sync.Mutex
	free     []placement.Candidate
	counts   map[reservation.Status]int
	freeErr  error
	countErr error
	block    bool

	freeCalls  int
	countCalls int
	start, end time.Time
	deadline   time.Time
}

func (f *fakeCapacity) FreeCandidates(ctx context.Context, start, end time.Time) ([]placement.Candidate, error) {
	f.mu.Lock()
	f.freeCalls++
	f.start, f.end = start, end
	f.deadline, _ = ctx.Deadline()
	free, err := f.free, f.freeErr
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return free, err
}

func (f *fakeCapacity) CountReservationsByStatus(ctx context.Context) (map[reservation.Status]int, error) {
	f.mu.Lock()
	f.countCalls++
	f.deadline, _ = ctx.Deadline()
	counts, err := f.counts, f.countErr
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return counts, err
}

func (f *fakeCapacity) setFree(free ...placement.Candidate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.free = free
}

func fixedClock() time.Time { return testNow }

func newCapacityMetrics(t *testing.T, src CapacitySource) *Metrics {
	t.Helper()
	m := newTestMetrics(t, &fakeCounter{})
	if err := m.registerCapacity(src, fixedClock); err != nil {
		t.Fatalf("registerCapacity: %v", err)
	}
	return m
}

func cand(id, pod, rack string) placement.Candidate {
	return placement.Candidate{NodeID: id, Pod: pod, Rack: rack}
}

func gatherOne(t *testing.T, c prometheus.Collector) (int, error) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	return len(mfs), err
}

func TestFreeNodesCountsCandidatesPerPod(t *testing.T) {
	src := &fakeCapacity{free: []placement.Candidate{
		cand("n-1", "p1", "r1"), cand("n-2", "p1", "r1"), cand("n-9", "p1", "r2"),
		cand("n-33", "p2", "r5"),
	}}
	c := newFreeNodesCollector(src, fixedClock)

	want := family("capacity_free_nodes", freeNodesHelp, "gauge",
		`capacity_free_nodes{pod="p1"} 3`,
		`capacity_free_nodes{pod="p2"} 1`,
	)

	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "capacity_free_nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestFreeNodesAsksForTheNextMinuteFromTheInjectedClock(t *testing.T) {
	src := &fakeCapacity{}
	c := newFreeNodesCollector(src, fixedClock)

	if _, err := gatherOne(t, c); err != nil {
		t.Fatal(err)
	}

	if !src.start.Equal(testNow) || !src.end.Equal(testNow.Add(time.Minute)) {
		t.Errorf("window = [%v, %v), want [%v, %v)", src.start, src.end, testNow, testNow.Add(time.Minute))
	}
}

func TestFreeNodesWithNoCandidatesHasNoSeries(t *testing.T) {
	c := newFreeNodesCollector(&fakeCapacity{}, fixedClock)

	if err := testutil.CollectAndCompare(c, strings.NewReader(""), "capacity_free_nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestFreeNodesPodThatFillsUpReportsZeroInsteadOfVanishing(t *testing.T) {
	src := &fakeCapacity{}
	src.setFree(cand("n-1", "p1", "r1"), cand("n-33", "p2", "r5"))
	c := newFreeNodesCollector(src, fixedClock)
	if _, err := gatherOne(t, c); err != nil {
		t.Fatal(err)
	}

	src.setFree(cand("n-33", "p2", "r5"))

	want := family("capacity_free_nodes", freeNodesHelp, "gauge",
		`capacity_free_nodes{pod="p1"} 0`,
		`capacity_free_nodes{pod="p2"} 1`,
	)
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "capacity_free_nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestFreeNodesFailedQueryDoesNotForgetPods(t *testing.T) {
	src := &fakeCapacity{free: []placement.Candidate{cand("n-1", "p1", "r1")}}
	c := newFreeNodesCollector(src, fixedClock)
	if _, err := gatherOne(t, c); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.freeErr = errors.New("db down")
	src.mu.Unlock()
	if _, err := gatherOne(t, c); err == nil {
		t.Fatal("want the failed scrape to error")
	}
	src.mu.Lock()
	src.freeErr, src.free = nil, nil
	src.mu.Unlock()

	want := family("capacity_free_nodes", freeNodesHelp, "gauge", `capacity_free_nodes{pod="p1"} 0`)
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "capacity_free_nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestFreeNodesQueryErrorBecomesScrapeErrorWithoutSeries(t *testing.T) {
	src := &fakeCapacity{freeErr: errors.New("db down"), free: []placement.Candidate{cand("n-1", "p1", "r1")}}

	families, err := gatherOne(t, newFreeNodesCollector(src, fixedClock))

	if err == nil || !strings.Contains(err.Error(), "list free candidates: db down") {
		t.Fatalf("Gather error = %v, want the wrapped query error", err)
	}
	if families != 0 {
		t.Errorf("%d families reported after a failed query, want none", families)
	}
}

func TestFreeNodesQueryRunsOncePerScrapeWithDeadline(t *testing.T) {
	src := &fakeCapacity{}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(newFreeNodesCollector(src, fixedClock)); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := reg.Gather(); err != nil {
			t.Fatal(err)
		}
	}

	if src.freeCalls != 3 {
		t.Errorf("queries = %d, want 3, one per scrape", src.freeCalls)
	}
	if src.deadline.IsZero() {
		t.Error("query context has no deadline")
	}
}

func TestFreeNodesQueryIsBoundedByTimeout(t *testing.T) {
	c := newFreeNodesCollector(&fakeCapacity{block: true}, fixedClock)
	c.timeout = 20 * time.Millisecond

	start := time.Now()
	_, err := gatherOne(t, c)

	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Gather error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Gather took %v, the timeout did not bound it", elapsed)
	}
}

func TestDefaultCapacityTimeoutIsTheNamedConstant(t *testing.T) {
	if got := newFreeNodesCollector(&fakeCapacity{}, fixedClock).timeout; got != capacityQueryTimeout {
		t.Errorf("free nodes timeout = %v, want %v", got, capacityQueryTimeout)
	}
	if got := newReservationsCollector(&fakeCapacity{}).timeout; got != capacityQueryTimeout {
		t.Errorf("reservations timeout = %v, want %v", got, capacityQueryTimeout)
	}
}

func TestReservationsEmitsAllStatusesWithZeros(t *testing.T) {
	src := &fakeCapacity{counts: map[reservation.Status]int{
		reservation.StatusScheduled: 3, reservation.StatusDegraded: 1, reservation.StatusCancelled: 7,
	}}

	want := family("reservations", reservationsHelp, "gauge",
		`reservations{status="scheduled"} 3`,
		`reservations{status="active"} 0`,
		`reservations{status="degraded"} 1`,
		`reservations{status="ended"} 0`,
		`reservations{status="cancelled"} 7`,
	)

	if err := testutil.CollectAndCompare(newReservationsCollector(src), strings.NewReader(want), "reservations"); err != nil {
		t.Fatal(err)
	}
}

func TestReservationsNoneIsAllZeros(t *testing.T) {
	want := family("reservations", reservationsHelp, "gauge",
		`reservations{status="scheduled"} 0`,
		`reservations{status="active"} 0`,
		`reservations{status="degraded"} 0`,
		`reservations{status="ended"} 0`,
		`reservations{status="cancelled"} 0`,
	)

	if err := testutil.CollectAndCompare(newReservationsCollector(&fakeCapacity{}), strings.NewReader(want), "reservations"); err != nil {
		t.Fatal(err)
	}
}

func TestReservationsIgnoresStatusesOutsideTheContract(t *testing.T) {
	src := &fakeCapacity{counts: map[reservation.Status]int{"bogus": 4, reservation.StatusActive: 2}}
	c := newReservationsCollector(src)

	if got := testutil.CollectAndCount(c, "reservations"); got != 5 {
		t.Fatalf("series = %d, want 5 (one per contract status)", got)
	}
}

func TestReservationsQueryErrorBecomesScrapeErrorWithoutSeries(t *testing.T) {
	src := &fakeCapacity{countErr: errors.New("db down")}

	families, err := gatherOne(t, newReservationsCollector(src))

	if err == nil || !strings.Contains(err.Error(), "count reservations by status: db down") {
		t.Fatalf("Gather error = %v, want the wrapped query error", err)
	}
	if families != 0 {
		t.Errorf("%d families reported after a failed query, want none", families)
	}
}

func TestReservationsQueryIsBoundedByTimeout(t *testing.T) {
	c := newReservationsCollector(&fakeCapacity{block: true})
	c.timeout = 20 * time.Millisecond

	_, err := gatherOne(t, c)

	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Gather error = %v, want deadline exceeded", err)
	}
}

func TestPlacementFailuresStartAtZeroForEveryPolicy(t *testing.T) {
	m := newCapacityMetrics(t, &fakeCapacity{})

	gatherAndCompare(t, m, family("placement_failures_total", failuresHelp, "counter",
		`placement_failures_total{policy="any"} 0`,
		`placement_failures_total{policy="same_pod"} 0`,
		`placement_failures_total{policy="same_rack"} 0`,
	), "placement_failures_total")
}

func TestPlacementFailedCountsPerPolicy(t *testing.T) {
	m := newCapacityMetrics(t, &fakeCapacity{})
	r := m.Reservations()

	r.PlacementFailed(placement.PolicySameRack)
	r.PlacementFailed(placement.PolicySameRack)
	r.PlacementFailed(placement.PolicyAny)

	gatherAndCompare(t, m, family("placement_failures_total", failuresHelp, "counter",
		`placement_failures_total{policy="any"} 1`,
		`placement_failures_total{policy="same_pod"} 0`,
		`placement_failures_total{policy="same_rack"} 2`,
	), "placement_failures_total")
}

func TestReservationDegradedStartsAtZeroAndCounts(t *testing.T) {
	m := newCapacityMetrics(t, &fakeCapacity{})
	gatherAndCompare(t, m, family("reservation_degraded_total", degradedHelp, "counter",
		`reservation_degraded_total 0`), "reservation_degraded_total")

	m.Reservations().Degraded()
	m.Reservations().Degraded()

	gatherAndCompare(t, m, family("reservation_degraded_total", degradedHelp, "counter",
		`reservation_degraded_total 2`), "reservation_degraded_total")
}

func TestReservationsSinkIsTheSameOnEveryCall(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})

	if m.Reservations() != m.Reservations() {
		t.Fatal("Reservations() returned different sinks")
	}
}

func TestCapacityMetricsAreAbsentUntilRegistered(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})

	_, body := scrape(t, m)

	for _, name := range []string{"capacity_free_nodes", "reservations", "placement_failures_total", "reservation_degraded_total"} {
		if strings.Contains(body, name) {
			t.Errorf("%s exposed without RegisterCapacity", name)
		}
	}
}

func TestSinkIsSafeBeforeRegisterCapacityAndKeepsItsCounts(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	r := m.Reservations()

	r.PlacementFailed(placement.PolicySamePod)
	r.Degraded()
	r.Degraded()
	if err := m.registerCapacity(&fakeCapacity{}, fixedClock); err != nil {
		t.Fatal(err)
	}

	gatherAndCompare(t, m, family("placement_failures_total", failuresHelp, "counter",
		`placement_failures_total{policy="any"} 0`,
		`placement_failures_total{policy="same_pod"} 1`,
		`placement_failures_total{policy="same_rack"} 0`,
	)+family("reservation_degraded_total", degradedHelp, "counter",
		`reservation_degraded_total 2`,
	), "placement_failures_total", "reservation_degraded_total")
}

func TestRegisterCapacityTwiceFailsWithoutDuplicatingSeries(t *testing.T) {
	m := newCapacityMetrics(t, &fakeCapacity{})
	m.Reservations().Degraded()

	err := m.RegisterCapacity(&fakeCapacity{})

	var already prometheus.AlreadyRegisteredError
	if !errors.As(err, &already) {
		t.Fatalf("second RegisterCapacity error = %v, want AlreadyRegisteredError", err)
	}
	gatherAndCompare(t, m, family("reservation_degraded_total", degradedHelp, "counter",
		`reservation_degraded_total 1`), "reservation_degraded_total")
}

func TestRegisterCapacityRejectsNilSource(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})

	if err := m.RegisterCapacity(nil); err == nil {
		t.Fatal("RegisterCapacity(nil) succeeded")
	}
	if _, body := scrape(t, m); strings.Contains(body, "capacity_free_nodes") {
		t.Error("capacity metrics registered despite the error")
	}
}

func TestHandlerServesCapacityFamilies(t *testing.T) {
	src := &fakeCapacity{
		free:   []placement.Candidate{cand("n-1", "p1", "r1")},
		counts: map[reservation.Status]int{reservation.StatusActive: 2},
	}
	m := newCapacityMetrics(t, src)
	m.Reservations().PlacementFailed(placement.PolicyAny)
	m.Reservations().Degraded()

	_, body := scrape(t, m)

	for _, want := range []string{
		"# TYPE capacity_free_nodes gauge",
		`capacity_free_nodes{pod="p1"} 1`,
		"# TYPE reservations gauge",
		`reservations{status="active"} 2`,
		`reservations{status="ended"} 0`,
		"# TYPE placement_failures_total counter",
		`placement_failures_total{policy="any"} 1`,
		`placement_failures_total{policy="same_rack"} 0`,
		"# TYPE reservation_degraded_total counter",
		"reservation_degraded_total 1",
		"fleet_nodes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

func TestHandlerKeepsServingOtherFamiliesWhenCapacityQueriesFail(t *testing.T) {
	src := &fakeCapacity{freeErr: errors.New("db down"), countErr: errors.New("db down")}
	m := newCapacityMetrics(t, src)
	m.Reservations().Degraded()

	res, body := scrape(t, m)

	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 with partial metrics", res.StatusCode)
	}
	for _, gone := range []string{"capacity_free_nodes", "reservations{"} {
		if strings.Contains(body, gone) {
			t.Errorf("%s served although its query failed", gone)
		}
	}
	for _, want := range []string{"reservation_degraded_total 1", "placement_failures_total", "fleet_nodes", "go_goroutines"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

func TestCapacityLabelValuesMatchTheContract(t *testing.T) {
	gotPolicies := make([]string, 0, len(placementPolicies))
	for _, p := range placementPolicies {
		gotPolicies = append(gotPolicies, string(p))
	}
	if want := "same_rack,same_pod,any"; strings.Join(gotPolicies, ",") != want {
		t.Errorf("policies = %v, want %s", gotPolicies, want)
	}
	gotStatuses := make([]string, 0, len(reservation.Statuses()))
	for _, s := range reservation.Statuses() {
		gotStatuses = append(gotStatuses, string(s))
	}
	if want := "scheduled,active,degraded,ended,cancelled"; strings.Join(gotStatuses, ",") != want {
		t.Errorf("statuses = %v, want %s", gotStatuses, want)
	}
}

func TestRegisterCapacityDoesNotTouchTheDefaultRegistry(t *testing.T) {
	newCapacityMetrics(t, &fakeCapacity{})

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	for _, mf := range mfs {
		switch name := mf.GetName(); name {
		case "capacity_free_nodes", "reservations", "placement_failures_total", "reservation_degraded_total":
			t.Errorf("%s leaked into the default registry", name)
		}
	}
}

func TestSinkAndScrapesAreRaceFree(t *testing.T) {
	m := newCapacityMetrics(t, &fakeCapacity{free: []placement.Candidate{cand("n-1", "p1", "r1")}})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				m.Reservations().PlacementFailed(placement.PolicyAny)
				m.Reservations().Degraded()
				scrape(t, m)
			}
		})
	}
	wg.Wait()

	gatherAndCompare(t, m, family("reservation_degraded_total", degradedHelp, "counter",
		`reservation_degraded_total 200`), "reservation_degraded_total")
}
