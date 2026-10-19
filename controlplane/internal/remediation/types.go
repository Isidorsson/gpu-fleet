// Package remediation turns health alerts into node lifecycle actions. See
// docs/remediation.md.
package remediation

import (
	"context"
	"errors"
	"time"
)

// Webhook is Alertmanager's webhook payload, version 4.
type Webhook struct {
	Version string  `json:"version"`
	Status  string  `json:"status"`
	Alerts  []Alert `json:"alerts"`
}

type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	Fingerprint string            `json:"fingerprint"`
}

const (
	WebhookVersion = "4"
	StatusFiring   = "firing"
	LabelAlertName = "alertname"
	LabelNodeID    = "node_id"
)

type Action string

const ActionQuarantine Action = "quarantine"

type Outcome string

const (
	OutcomeQuarantined        Outcome = "quarantined"
	OutcomeAlreadyQuarantined Outcome = "already_quarantined"
	OutcomeNotApplicable      Outcome = "not_applicable"
	OutcomeUnknownNode        Outcome = "unknown_node"
	OutcomeRateLimited        Outcome = "rate_limited"
	OutcomeIgnored            Outcome = "ignored"
)

// Outcomes returns every outcome, for metrics pre-initialization and checks.
func Outcomes() []Outcome {
	return []Outcome{
		OutcomeQuarantined, OutcomeAlreadyQuarantined, OutcomeNotApplicable,
		OutcomeUnknownNode, OutcomeRateLimited, OutcomeIgnored,
	}
}

// Decision is what Decide asks the store to do.
type Decision struct {
	NodeID      string
	AlertName   string
	Fingerprint string
	Action      Action
}

// Limit caps remediation quarantines: at most Max within Window.
type Limit struct {
	Max    int
	Window time.Duration
}

var DefaultLimit = Limit{Max: 5, Window: 10 * time.Minute}

// Record is one row of the remediation audit log.
type Record struct {
	NodeID      string
	AlertName   string
	Fingerprint string
	Action      Action
	Outcome     Outcome
	At          time.Time
}

// Summary counts outcomes for one webhook call.
type Summary struct {
	Outcomes map[Outcome]int
}

var ErrUnsupportedVersion = errors.New("unsupported alertmanager webhook version")

// Store is implemented by *store.Postgres.
type Store interface {
	ApplyRemediation(ctx context.Context, d Decision, limit Limit, now time.Time) (Outcome, error)
}

// Metrics is reported to once per handled alert. A no-op is used when nil.
type Metrics interface {
	Remediated(o Outcome)
}

type NopMetrics struct{}

func (NopMetrics) Remediated(Outcome) {}
