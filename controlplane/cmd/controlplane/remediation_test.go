package main

import "testing"

func TestNewRemediationRequiresToken(t *testing.T) {
	t.Setenv("REMEDIATION_WEBHOOK_TOKEN", "")
	if _, err := newRemediation(nil, nil, nil); err == nil {
		t.Fatal("want error without REMEDIATION_WEBHOOK_TOKEN")
	}
}
