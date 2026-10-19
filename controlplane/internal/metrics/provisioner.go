package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gpu-fleet/controlplane/internal/attest"
	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/provisioner"
)

const (
	labelState   = "state"
	labelTask    = "task"
	labelOutcome = "outcome"
	labelOp      = "op"
	labelResult  = "result"
	labelEvent   = "event"
	labelVerdict = "verdict"
)

// taskDurationBuckets (seconds) span a fast fake task up to the 10 minute
// default task timeout and a little beyond.
var taskDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200}

// provisionerMetrics implements provisioner.Metrics, so the provisioner never
// depends on Prometheus.
type provisionerMetrics struct {
	tasks     *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	bmc       *prometheus.CounterVec
	events    *prometheus.CounterVec
	attested  *prometheus.CounterVec
	leaseLost prometheus.Counter
}

var _ provisioner.Metrics = (*provisionerMetrics)(nil)

// quarantineCauses and verdicts start at 0 so increase() in the QuarantineSpike
// and AttestationFailure alerts sees the first occurrence; a series born at 1
// has no prior sample to diff against.
var quarantineCauses = []node.Event{
	node.EventInspectionFailed, node.EventAttestationFailed, node.EventWipeFailed, node.EventHardwareFault,
}

var verdicts = []attest.Verdict{attest.VerdictPass, attest.VerdictFail}

func newProvisionerMetrics() *provisionerMetrics {
	m := newProvisionerCollectors()
	for _, ev := range quarantineCauses {
		m.events.WithLabelValues(string(ev))
	}
	for _, v := range verdicts {
		m.attested.WithLabelValues(string(v))
	}
	return m
}

func newProvisionerCollectors() *provisionerMetrics {
	return &provisionerMetrics{
		tasks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "provisioner_tasks_total",
			Help: "Tasks run by provisioner workers, by task and outcome.",
		}, []string{labelTask, labelOutcome}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "provisioner_task_duration_seconds",
			Help:    "Time from the start of a task to its result, in seconds.",
			Buckets: taskDurationBuckets,
		}, []string{labelTask}),
		bmc: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "provisioner_bmc_requests_total",
			Help: "BMC and agent call attempts, retries included.",
		}, []string{labelOp, labelResult}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "provisioner_events_total",
			Help: "Lifecycle events applied by provisioner workers.",
		}, []string{labelEvent}),
		attested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "attestations_total",
			Help: "Node attestations run by provisioner workers, by verdict.",
		}, []string{labelVerdict}),
		leaseLost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "provisioner_lease_lost_total",
			Help: "Sessions abandoned because the node lease was lost.",
		}),
	}
}

func (p *provisionerMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{p.tasks, p.duration, p.bmc, p.events, p.attested, p.leaseLost}
}

func (p *provisionerMetrics) TaskFinished(task deployagent.Task, outcome provisioner.Outcome, d time.Duration) {
	p.tasks.WithLabelValues(string(task), string(outcome)).Inc()
	p.duration.WithLabelValues(string(task)).Observe(d.Seconds())
}

func (p *provisionerMetrics) BMCRequest(op provisioner.BMCOp, result provisioner.CallResult) {
	p.bmc.WithLabelValues(string(op), string(result)).Inc()
}

func (p *provisionerMetrics) EventApplied(ev node.Event) {
	p.events.WithLabelValues(string(ev)).Inc()
}

func (p *provisionerMetrics) Attested(v attest.Verdict) {
	p.attested.WithLabelValues(string(v)).Inc()
}

func (p *provisionerMetrics) LeaseLost() { p.leaseLost.Inc() }
