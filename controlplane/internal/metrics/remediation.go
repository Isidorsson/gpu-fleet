package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"gpu-fleet/controlplane/internal/remediation"
)

// remediationMetrics implements remediation.Metrics, so the remediation
// package never depends on Prometheus.
type remediationMetrics struct {
	actions *prometheus.CounterVec
}

var _ remediation.Metrics = (*remediationMetrics)(nil)

// Outcomes start at 0 so increase() in the RemediationRateLimited alert sees
// the first rate-limited alert; a series born at 1 has no prior sample to
// diff against.
func newRemediationMetrics() *remediationMetrics {
	m := &remediationMetrics{
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "remediation_actions_total",
			Help: "Alerts handled by the remediation webhook, by outcome.",
		}, []string{labelOutcome}),
	}
	for _, o := range remediation.Outcomes() {
		m.actions.WithLabelValues(string(o))
	}
	return m
}

func (r *remediationMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{r.actions}
}

func (r *remediationMetrics) Remediated(o remediation.Outcome) {
	r.actions.WithLabelValues(string(o)).Inc()
}
