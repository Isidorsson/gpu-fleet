package remediation

import (
	"errors"
	"fmt"
)

const alertGPUFatalXid = "GPUFatalXid"

// rules maps an alertname to the action it asks for. An alertname without an
// entry is ignored.
var rules = map[string]Action{
	alertGPUFatalXid: ActionQuarantine,
}

var ErrInvalidConfig = errors.New("invalid remediation config")

// Decide turns an alert into a decision, without I/O. Only a firing alert with
// a rule and a node_id yields a Decision, and then the Outcome is empty: it is
// the store's to settle. Every other alert gets the zero Decision and
// OutcomeIgnored. A resolved alert never undoes a quarantine: repairs are a
// human decision.
func Decide(a Alert) (Decision, Outcome) {
	if a.Status != StatusFiring {
		return Decision{}, OutcomeIgnored
	}
	alertName := a.Labels[LabelAlertName]
	action, ok := rules[alertName]
	if !ok {
		return Decision{}, OutcomeIgnored
	}
	nodeID := a.Labels[LabelNodeID]
	if nodeID == "" {
		return Decision{}, OutcomeIgnored
	}
	return Decision{NodeID: nodeID, AlertName: alertName, Fingerprint: a.Fingerprint, Action: action}, ""
}

// Validate reports whether l can cap anything: at least one quarantine must be
// allowed in a window of positive length.
func (l Limit) Validate() error {
	if l.Max < 1 {
		return fmt.Errorf("%w: limit Max must be at least 1, got %d", ErrInvalidConfig, l.Max)
	}
	if l.Window <= 0 {
		return fmt.Errorf("%w: limit Window must be positive, got %s", ErrInvalidConfig, l.Window)
	}
	return nil
}
