package metrics

import (
	"testing"

	"gpu-fleet/controlplane/internal/remediation"
)

const remediationHelp = "Alerts handled by the remediation webhook, by outcome."

func TestRemediationActionsStartAtZeroForEveryOutcome(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})

	gatherAndCompare(t, m, family("remediation_actions_total", remediationHelp, "counter",
		`remediation_actions_total{outcome="already_quarantined"} 0`,
		`remediation_actions_total{outcome="ignored"} 0`,
		`remediation_actions_total{outcome="not_applicable"} 0`,
		`remediation_actions_total{outcome="quarantined"} 0`,
		`remediation_actions_total{outcome="rate_limited"} 0`,
		`remediation_actions_total{outcome="unknown_node"} 0`,
	), "remediation_actions_total")
}

func TestRemediatedCountsPerOutcome(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	r := m.Remediation()

	r.Remediated(remediation.OutcomeQuarantined)
	r.Remediated(remediation.OutcomeQuarantined)
	r.Remediated(remediation.OutcomeQuarantined)
	r.Remediated(remediation.OutcomeRateLimited)
	r.Remediated(remediation.OutcomeIgnored)

	gatherAndCompare(t, m, family("remediation_actions_total", remediationHelp, "counter",
		`remediation_actions_total{outcome="already_quarantined"} 0`,
		`remediation_actions_total{outcome="ignored"} 1`,
		`remediation_actions_total{outcome="not_applicable"} 0`,
		`remediation_actions_total{outcome="quarantined"} 3`,
		`remediation_actions_total{outcome="rate_limited"} 1`,
		`remediation_actions_total{outcome="unknown_node"} 0`,
	), "remediation_actions_total")
}

func TestRemediationReturnsOneSinkWhoseCountsAreServedFromNew(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})

	m.Remediation().Remediated(remediation.OutcomeUnknownNode)
	m.Remediation().Remediated(remediation.OutcomeUnknownNode)
	m.Remediation().Remediated(remediation.OutcomeAlreadyQuarantined)
	m.Remediation().Remediated(remediation.OutcomeNotApplicable)

	gatherAndCompare(t, m, family("remediation_actions_total", remediationHelp, "counter",
		`remediation_actions_total{outcome="already_quarantined"} 1`,
		`remediation_actions_total{outcome="ignored"} 0`,
		`remediation_actions_total{outcome="not_applicable"} 1`,
		`remediation_actions_total{outcome="quarantined"} 0`,
		`remediation_actions_total{outcome="rate_limited"} 0`,
		`remediation_actions_total{outcome="unknown_node"} 2`,
	), "remediation_actions_total")
}

func TestRemediationOutcomeLabelValuesMatchTheContract(t *testing.T) {
	tests := []struct{ got, want string }{
		{string(remediation.OutcomeQuarantined), "quarantined"},
		{string(remediation.OutcomeAlreadyQuarantined), "already_quarantined"},
		{string(remediation.OutcomeNotApplicable), "not_applicable"},
		{string(remediation.OutcomeUnknownNode), "unknown_node"},
		{string(remediation.OutcomeRateLimited), "rate_limited"},
		{string(remediation.OutcomeIgnored), "ignored"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("label value = %q, want %q", tc.got, tc.want)
		}
	}
	if got := len(remediation.Outcomes()); got != len(tests) {
		t.Errorf("%d outcomes, want %d", got, len(tests))
	}
}
