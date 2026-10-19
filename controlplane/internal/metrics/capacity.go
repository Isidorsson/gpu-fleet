package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

const (
	labelPod    = "pod"
	labelStatus = "status"
	labelPolicy = "policy"
)

const (
	// capacityQueryTimeout bounds each per-scrape capacity query; it shares the
	// scrape budget with the fleet query.
	capacityQueryTimeout = fleetQueryTimeout
	// capacityWindow is how far ahead capacity_free_nodes looks: nodes free
	// right now and for the next minute.
	capacityWindow = time.Minute
)

var placementPolicies = []placement.Policy{
	placement.PolicySameRack,
	placement.PolicySamePod,
	placement.PolicyAny,
}

type freeLister interface {
	FreeCandidates(ctx context.Context, start, end time.Time) ([]placement.Candidate, error)
}

type statusCounter interface {
	CountReservationsByStatus(ctx context.Context) (map[reservation.Status]int, error)
}

// CapacitySource is what the capacity gauges are computed from.
// *store.Postgres satisfies it.
type CapacitySource interface {
	freeLister
	statusCounter
}

// freeNodesCollector reports capacity_free_nodes. FreeCandidates only returns
// free nodes, so a pod that is fully booked would vanish from the result;
// pods seen on an earlier scrape keep reporting 0 instead of disappearing.
type freeNodesCollector struct {
	src     freeLister
	clock   func() time.Time
	timeout time.Duration
	desc    *prometheus.Desc

	mu   sync.Mutex
	seen map[string]struct{}
}

func newFreeNodesCollector(src freeLister, clock func() time.Time) *freeNodesCollector {
	return &freeNodesCollector{
		src:     src,
		clock:   clock,
		timeout: capacityQueryTimeout,
		desc: prometheus.NewDesc("capacity_free_nodes",
			"Nodes free for the next minute, by pod, counted at scrape time.", []string{labelPod}, nil),
		seen: map[string]struct{}{},
	}
}

func (c *freeNodesCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *freeNodesCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	start := c.clock()
	free, err := c.src.FreeCandidates(ctx, start, start.Add(capacityWindow))
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, fmt.Errorf("list free candidates: %w", err))
		return
	}
	for pod, n := range c.countByPod(free) {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), pod)
	}
}

func (c *freeNodesCollector) countByPod(free []placement.Candidate) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := make(map[string]int, len(c.seen))
	for pod := range c.seen {
		counts[pod] = 0
	}
	for _, cand := range free {
		counts[cand.Pod]++
	}
	for pod := range counts {
		c.seen[pod] = struct{}{}
	}
	return counts
}

// reservationsCollector reports reservations, counted at scrape time.
type reservationsCollector struct {
	counter statusCounter
	timeout time.Duration
	desc    *prometheus.Desc
}

func newReservationsCollector(counter statusCounter) *reservationsCollector {
	return &reservationsCollector{
		counter: counter,
		timeout: capacityQueryTimeout,
		desc: prometheus.NewDesc("reservations",
			"Reservations by status, counted at scrape time.", []string{labelStatus}, nil),
	}
}

func (c *reservationsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *reservationsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	counts, err := c.counter.CountReservationsByStatus(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, fmt.Errorf("count reservations by status: %w", err))
		return
	}
	for _, st := range reservation.Statuses() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[st]), string(st))
	}
}

// reservationMetrics implements reservation.Metrics, so the API and reconciler
// never depend on Prometheus. The counters are created with the Metrics but
// only exposed once RegisterCapacity registers them; counts made before that
// are kept.
type reservationMetrics struct {
	failures *prometheus.CounterVec
	degraded prometheus.Counter
}

var _ reservation.Metrics = (*reservationMetrics)(nil)

// Policies start at 0 so increase() sees the first failure of each; a series
// born at 1 has no prior sample to diff against.
func newReservationMetrics() *reservationMetrics {
	m := &reservationMetrics{
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "placement_failures_total",
			Help: "Requests rejected for insufficient capacity, by policy.",
		}, []string{labelPolicy}),
		degraded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservation_degraded_total",
			Help: "Reservations that started with fewer nodes than requested.",
		}),
	}
	for _, p := range placementPolicies {
		m.failures.WithLabelValues(string(p))
	}
	return m
}

func (r *reservationMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{r.failures, r.degraded}
}

func (r *reservationMetrics) PlacementFailed(policy placement.Policy) {
	r.failures.WithLabelValues(string(policy)).Inc()
}

func (r *reservationMetrics) Degraded() { r.degraded.Inc() }
