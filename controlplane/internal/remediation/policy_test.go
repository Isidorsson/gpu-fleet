package remediation

import (
	"errors"
	"testing"
	"time"
)

func alert(status string, labels map[string]string) Alert {
	return Alert{Status: status, Labels: labels, Fingerprint: "fp-1"}
}

func TestDecide(t *testing.T) {
	quarantineNode3 := Decision{NodeID: "node-003", AlertName: "GPUFatalXid", Fingerprint: "fp-1", Action: ActionQuarantine}
	tests := []struct {
		name         string
		alert        Alert
		wantDecision Decision
		wantOutcome  Outcome
	}{
		{
			name:         "firing GPUFatalXid with node_id quarantines that node",
			alert:        alert("firing", map[string]string{"alertname": "GPUFatalXid", "node_id": "node-003", "gpu": "3"}),
			wantDecision: quarantineNode3,
		},
		{
			name:        "resolved alert is ignored",
			alert:       alert("resolved", map[string]string{"alertname": "GPUFatalXid", "node_id": "node-003"}),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "empty status is ignored",
			alert:       alert("", map[string]string{"alertname": "GPUFatalXid", "node_id": "node-003"}),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "alertname without a rule is ignored",
			alert:       alert("firing", map[string]string{"alertname": "GPUHot", "node_id": "node-003"}),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "missing alertname is ignored",
			alert:       alert("firing", map[string]string{"node_id": "node-003"}),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "missing node_id is ignored",
			alert:       alert("firing", map[string]string{"alertname": "GPUFatalXid"}),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "empty node_id is ignored",
			alert:       alert("firing", map[string]string{"alertname": "GPUFatalXid", "node_id": ""}),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "nil labels are ignored",
			alert:       alert("firing", nil),
			wantOutcome: OutcomeIgnored,
		},
		{
			name:        "alertname match is case sensitive",
			alert:       alert("firing", map[string]string{"alertname": "gpufatalxid", "node_id": "node-003"}),
			wantOutcome: OutcomeIgnored,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDecision, gotOutcome := Decide(tt.alert)
			if gotDecision != tt.wantDecision || gotOutcome != tt.wantOutcome {
				t.Fatalf("Decide = (%+v, %q), want (%+v, %q)", gotDecision, gotOutcome, tt.wantDecision, tt.wantOutcome)
			}
		})
	}
}

func TestOnlyFatalXidHasARule(t *testing.T) {
	if len(rules) != 1 || rules["GPUFatalXid"] != ActionQuarantine {
		t.Fatalf("rules = %v, want only GPUFatalXid -> quarantine", rules)
	}
}

func TestLimitValidate(t *testing.T) {
	tests := []struct {
		name  string
		limit Limit
		ok    bool
	}{
		{"default", DefaultLimit, true},
		{"one per nanosecond", Limit{Max: 1, Window: time.Nanosecond}, true},
		{"zero max", Limit{Max: 0, Window: time.Minute}, false},
		{"negative max", Limit{Max: -1, Window: time.Minute}, false},
		{"zero window", Limit{Max: 5, Window: 0}, false},
		{"negative window", Limit{Max: 5, Window: -time.Minute}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.limit.Validate()
			if (err == nil) != tt.ok {
				t.Fatalf("Validate(%+v) = %v, want ok=%v", tt.limit, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate(%+v) = %v, want ErrInvalidConfig", tt.limit, err)
			}
		})
	}
}
