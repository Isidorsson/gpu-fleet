// Package metrics owns the control plane's Prometheus registry and everything
// registered on it. The contract is docs/metrics.md.
package metrics

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gpu-fleet/controlplane/internal/provisioner"
	"gpu-fleet/controlplane/internal/remediation"
	"gpu-fleet/controlplane/internal/reservation"
)

type Metrics struct {
	registry    *prometheus.Registry
	provisioner *provisionerMetrics
	reservation *reservationMetrics
	remediation *remediationMetrics
	handler     http.Handler
}

// New builds a private registry with the Go runtime and process collectors,
// fleet_nodes (counted through counter), the provisioner metrics and
// remediation_actions_total. Nothing touches the global default registry. The
// capacity metrics are added with RegisterCapacity.
func New(counter NodeCounter, log *slog.Logger) (*Metrics, error) {
	m := &Metrics{
		registry:    prometheus.NewRegistry(),
		provisioner: newProvisionerMetrics(),
		reservation: newReservationMetrics(),
		remediation: newRemediationMetrics(),
	}
	cs := []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newFleetCollector(counter),
	}
	cs = append(cs, m.provisioner.collectors()...)
	cs = append(cs, m.remediation.collectors()...)
	for _, c := range cs {
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

// Reservations returns the sink to pass to the reservation API and reconciler.
// It is safe to use before RegisterCapacity: counts made earlier show up once
// the capacity metrics are registered.
func (m *Metrics) Reservations() reservation.Metrics { return m.reservation }

// Remediation returns the sink to pass to the remediation service.
func (m *Metrics) Remediation() remediation.Metrics { return m.remediation }

// RegisterCapacity exposes capacity_free_nodes, reservations,
// placement_failures_total and reservation_degraded_total, the first two
// computed from src at scrape time. It can be called once.
func (m *Metrics) RegisterCapacity(src CapacitySource) error {
	return m.registerCapacity(src, time.Now)
}

func (m *Metrics) registerCapacity(src CapacitySource, clock func() time.Time) error {
	if src == nil {
		return errors.New("register capacity metrics: nil source")
	}
	cs := append([]prometheus.Collector{
		newFreeNodesCollector(src, clock),
		newReservationsCollector(src),
	}, m.reservation.collectors()...)
	for _, c := range cs {
		if err := m.registry.Register(c); err != nil {
			return fmt.Errorf("register capacity collector: %w", err)
		}
	}
	return nil
}
