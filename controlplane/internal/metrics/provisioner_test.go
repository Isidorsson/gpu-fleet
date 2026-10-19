package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/provisioner"
)

func newTestMetrics(t *testing.T, counter NodeCounter) *Metrics {
	t.Helper()
	m, err := New(counter, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// family renders one metric family in the text exposition format.
func family(name, help, kind string, samples ...string) string {
	return "# HELP " + name + " " + help + "\n# TYPE " + name + " " + kind + "\n" +
		strings.Join(samples, "\n") + "\n"
}

func gatherAndCompare(t *testing.T, m *Metrics, want string, names ...string) {
	t.Helper()
	if err := testutil.GatherAndCompare(m.registry, strings.NewReader(want), names...); err != nil {
		t.Fatal(err)
	}
}

func TestTasksTotalCountsPerTaskAndOutcome(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	p := m.Provisioner()

	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeSucceeded, time.Second)
	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeSucceeded, time.Second)
	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeFault, time.Second)
	p.TaskFinished(deployagent.TaskWipe, provisioner.OutcomeFailed, time.Second)
	p.TaskFinished(deployagent.TaskDeploy, provisioner.OutcomeAbandoned, time.Second)

	gatherAndCompare(t, m, family("provisioner_tasks_total",
		"Tasks run by provisioner workers, by task and outcome.", "counter",
		`provisioner_tasks_total{outcome="abandoned",task="deploy"} 1`,
		`provisioner_tasks_total{outcome="failed",task="wipe"} 1`,
		`provisioner_tasks_total{outcome="fault",task="inspect"} 1`,
		`provisioner_tasks_total{outcome="succeeded",task="inspect"} 2`,
	), "provisioner_tasks_total")
}

func TestTaskDurationHistogramBucketsAndSum(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	p := m.Provisioner()

	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeSucceeded, 500*time.Millisecond)
	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeFailed, 20*time.Second)
	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeFault, 2000*time.Second)

	gatherAndCompare(t, m, family("provisioner_task_duration_seconds",
		"Time from the start of a task to its result, in seconds.", "histogram",
		`provisioner_task_duration_seconds_bucket{task="inspect",le="1"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="5"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="15"} 1`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="30"} 2`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="60"} 2`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="120"} 2`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="300"} 2`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="600"} 2`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="1200"} 2`,
		`provisioner_task_duration_seconds_bucket{task="inspect",le="+Inf"} 3`,
		`provisioner_task_duration_seconds_sum{task="inspect"} 2020.5`,
		`provisioner_task_duration_seconds_count{task="inspect"} 3`,
	), "provisioner_task_duration_seconds")
}

func TestTaskDurationIsLabeledByTaskOnly(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	p := m.Provisioner()

	p.TaskFinished(deployagent.TaskWipe, provisioner.OutcomeSucceeded, time.Second)
	p.TaskFinished(deployagent.TaskDeploy, provisioner.OutcomeFailed, time.Second)

	if got := testutil.CollectAndCount(m.provisioner.duration, "provisioner_task_duration_seconds"); got != 2 {
		t.Fatalf("histogram series = %d, want 2 (wipe, deploy)", got)
	}
}

func TestBMCRequestsCountPerOpAndResult(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	p := m.Provisioner()

	p.BMCRequest(provisioner.OpSetTask, provisioner.ResultOK)
	p.BMCRequest(provisioner.OpSetPXE, provisioner.ResultTransient)
	p.BMCRequest(provisioner.OpSetPXE, provisioner.ResultTransient)
	p.BMCRequest(provisioner.OpReset, provisioner.ResultError)
	p.BMCRequest(provisioner.OpStatus, provisioner.ResultOK)

	gatherAndCompare(t, m, family("provisioner_bmc_requests_total",
		"BMC and agent call attempts, retries included.", "counter",
		`provisioner_bmc_requests_total{op="reset",result="error"} 1`,
		`provisioner_bmc_requests_total{op="set_pxe",result="transient"} 2`,
		`provisioner_bmc_requests_total{op="set_task",result="ok"} 1`,
		`provisioner_bmc_requests_total{op="status",result="ok"} 1`,
	), "provisioner_bmc_requests_total")
}

func TestEventsTotalCountsPerEvent(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	p := m.Provisioner()

	p.EventApplied(node.EventHardwareFault)
	p.EventApplied(node.EventHardwareFault)
	p.EventApplied(node.EventInspectionPassed)

	gatherAndCompare(t, m, family("provisioner_events_total",
		"Lifecycle events applied by provisioner workers.", "counter",
		`provisioner_events_total{event="hardware_fault"} 2`,
		`provisioner_events_total{event="inspection_failed"} 0`,
		`provisioner_events_total{event="inspection_passed"} 1`,
		`provisioner_events_total{event="wipe_failed"} 0`,
	), "provisioner_events_total")
}

func TestQuarantineCausesStartAtZero(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	gatherAndCompare(t, m, family("provisioner_events_total",
		"Lifecycle events applied by provisioner workers.", "counter",
		`provisioner_events_total{event="hardware_fault"} 0`,
		`provisioner_events_total{event="inspection_failed"} 0`,
		`provisioner_events_total{event="wipe_failed"} 0`,
	), "provisioner_events_total")
}

func TestLeaseLostTotalStartsAtZeroWithoutLabels(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	want := func(n string) string {
		return family("provisioner_lease_lost_total",
			"Sessions abandoned because the node lease was lost.", "counter",
			"provisioner_lease_lost_total "+n)
	}

	gatherAndCompare(t, m, want("0"), "provisioner_lease_lost_total")
	m.Provisioner().LeaseLost()
	m.Provisioner().LeaseLost()
	gatherAndCompare(t, m, want("2"), "provisioner_lease_lost_total")
}

func TestProvisionerLabelValuesMatchTheContract(t *testing.T) {
	tests := []struct{ got, want string }{
		{string(deployagent.TaskInspect), "inspect"},
		{string(deployagent.TaskWipe), "wipe"},
		{string(deployagent.TaskDeploy), "deploy"},
		{string(provisioner.OutcomeSucceeded), "succeeded"},
		{string(provisioner.OutcomeFailed), "failed"},
		{string(provisioner.OutcomeFault), "fault"},
		{string(provisioner.OutcomeAbandoned), "abandoned"},
		{string(provisioner.OpSetTask), "set_task"},
		{string(provisioner.OpSetPXE), "set_pxe"},
		{string(provisioner.OpReset), "reset"},
		{string(provisioner.OpStatus), "status"},
		{string(provisioner.ResultOK), "ok"},
		{string(provisioner.ResultTransient), "transient"},
		{string(provisioner.ResultError), "error"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("label value = %q, want %q", tc.got, tc.want)
		}
	}
}
