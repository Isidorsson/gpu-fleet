package provisioner

import (
	"time"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/node"
)

// Outcome is how a task ended. The values are the label values documented in
// docs/metrics.md.
type Outcome string

const (
	// OutcomeSucceeded: the agent reported success and the node moved on.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeFailed: the agent reported failure and the node moved on.
	OutcomeFailed Outcome = "failed"
	// OutcomeFault: the node was quarantined through hardware_fault.
	OutcomeFault Outcome = "fault"
	// OutcomeAbandoned: no event was applied because the lease was lost, an
	// operator changed the node, the worker shut down or the store failed.
	OutcomeAbandoned Outcome = "abandoned"
)

// BMCOp names the kind of BMC or agent call, independent of the argument: a
// force restart and a force off are both OpReset.
type BMCOp string

const (
	OpSetTask BMCOp = "set_task"
	OpSetPXE  BMCOp = "set_pxe"
	OpReset   BMCOp = "reset"
	OpStatus  BMCOp = "status"
)

// CallResult is how one call attempt ended.
type CallResult string

const (
	ResultOK        CallResult = "ok"
	ResultTransient CallResult = "transient"
	ResultError     CallResult = "error"
)

// Metrics receives what the workers do. Implementations must be safe for
// concurrent use.
type Metrics interface {
	// TaskFinished is called once per task, d being the time from the first
	// call to the end of the task as measured by the worker's Clock.
	TaskFinished(task deployagent.Task, outcome Outcome, d time.Duration)
	// BMCRequest is called for every call attempt, retries included.
	BMCRequest(op BMCOp, result CallResult)
	EventApplied(ev node.Event)
	// LeaseLost is called once per session abandoned because of a lost lease.
	LeaseLost()
}

type noopMetrics struct{}

func (noopMetrics) TaskFinished(deployagent.Task, Outcome, time.Duration) {}
func (noopMetrics) BMCRequest(BMCOp, CallResult)                          {}
func (noopMetrics) EventApplied(node.Event)                               {}
func (noopMetrics) LeaseLost()                                            {}

func resultOf(err error) CallResult {
	switch {
	case err == nil:
		return ResultOK
	case isTransient(err):
		return ResultTransient
	default:
		return ResultError
	}
}
