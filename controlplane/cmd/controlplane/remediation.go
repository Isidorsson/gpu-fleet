package main

import (
	"errors"
	"log/slog"
	"os"

	"gpu-fleet/controlplane/internal/remediation"
	"gpu-fleet/controlplane/internal/store"
)

type remediationSetup struct {
	service *remediation.Service
	token   string
}

// newRemediation builds the alert-driven remediation service. The webhook
// token is required: an unauthenticated POST /v1/alerts would let anyone
// quarantine nodes.
func newRemediation(db *store.Postgres, m remediation.Metrics, log *slog.Logger) (remediationSetup, error) {
	token := os.Getenv("REMEDIATION_WEBHOOK_TOKEN")
	if token == "" {
		return remediationSetup{}, errors.New("REMEDIATION_WEBHOOK_TOKEN is required")
	}
	svc, err := remediation.New(remediation.Config{}, remediation.Deps{Store: db, Metrics: m, Log: log})
	if err != nil {
		return remediationSetup{}, err
	}
	return remediationSetup{service: svc, token: token}, nil
}
