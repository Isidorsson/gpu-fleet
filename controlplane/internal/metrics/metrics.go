// Package metrics owns the control plane's Prometheus registry and everything
// registered on it. The contract is docs/metrics.md.
package metrics

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gpu-fleet/controlplane/internal/provisioner"
)

type Metrics struct {
	registry    *prometheus.Registry
	provisioner *provisionerMetrics
	handler     http.Handler
}

// New builds a private registry with the Go runtime and process collectors,
// fleet_nodes (counted through counter) and the provisioner metrics. Nothing
// touches the global default registry.
func New(counter NodeCounter, log *slog.Logger) (*Metrics, error) {
	m := &Metrics{registry: prometheus.NewRegistry(), provisioner: newProvisionerMetrics()}
	cs := []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newFleetCollector(counter),
	}
	for _, c := range append(cs, m.provisioner.collectors()...) {
		if err := m.registry.Register(c); err != nil {
			return nil, fmt.Errorf("register collector: %w", err)
		}
	}
	// A failing fleet query must not hide the other families, so errors are
	// logged and the scrape still serves what could be gathered.
	m.handler = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
	})
	return m, nil
}

// Handler serves the Prometheus text exposition.
func (m *Metrics) Handler() http.Handler { return m.handler }

// Provisioner returns the sink to pass as provisioner.Deps.Metrics.
func (m *Metrics) Provisioner() provisioner.Metrics { return m.provisioner }
