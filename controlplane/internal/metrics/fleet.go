package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gpu-fleet/controlplane/internal/node"
)

// fleetQueryTimeout bounds the per-scrape query; Prometheus gives a scrape 10s
// by default and the other collectors need their share of it.
const fleetQueryTimeout = 2 * time.Second

// NodeCounter counts nodes per state. States with no nodes may be absent.
type NodeCounter interface {
	CountByState(ctx context.Context) (map[node.State]int, error)
}

// fleetCollector reports fleet_nodes. It queries at scrape time instead of
// keeping a gauge in step with every transition, so the numbers are right
// even when another replica or the API made the change.
type fleetCollector struct {
	counter NodeCounter
	timeout time.Duration
	desc    *prometheus.Desc
}

func newFleetCollector(counter NodeCounter) *fleetCollector {
	return &fleetCollector{
		counter: counter,
		timeout: fleetQueryTimeout,
		desc: prometheus.NewDesc("fleet_nodes",
			"Nodes by lifecycle state, counted at scrape time.", []string{labelState}, nil),
	}
}

func (c *fleetCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect cannot take a context, so the query gets its own bounded one. A
// failed query is reported as an invalid metric: the scrape carries the error
// instead of stale or zero counts.
func (c *fleetCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	counts, err := c.counter.CountByState(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, fmt.Errorf("count nodes by state: %w", err))
		return
	}
	for _, st := range node.States() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[st]), string(st))
	}
}
