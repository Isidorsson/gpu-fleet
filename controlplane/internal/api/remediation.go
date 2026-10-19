package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpu-fleet/controlplane/internal/remediation"
)

const (
	defaultRemediationLimit = 50
	maxRemediationLimit     = 500
	bearerScheme            = "Bearer"
)

// Remediator handles one Alertmanager webhook. *remediation.Service
// implements it.
type Remediator interface {
	Handle(ctx context.Context, w remediation.Webhook) (remediation.Summary, error)
}

// RemediationLog lists the remediation audit log, newest first.
// *store.Postgres implements it.
type RemediationLog interface {
	ListRemediations(ctx context.Context, max int) ([]remediation.Record, error)
}

type remediationJSON struct {
	NodeID      string `json:"node_id"`
	AlertName   string `json:"alertname"`
	Fingerprint string `json:"fingerprint"`
	Action      string `json:"action"`
	Outcome     string `json:"outcome"`
	At          string `json:"at"`
}

func (s *server) receiveAlerts(w http.ResponseWriter, r *http.Request) {
	if !s.webhookAuthorized(r) {
		s.log.Warn("alert webhook rejected: missing or wrong bearer token", "remote", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", bearerScheme)
		s.writeError(w, errUnauthorized)
		return
	}
	hook, err := remediation.ParseWebhook(r.Body)
	if err != nil {
		s.writeError(w, fmt.Errorf("%w: %w", errBadReq, err))
		return
	}
	summary, err := s.remediator.Handle(r.Context(), hook)
	if err != nil {
		s.writeError(w, fmt.Errorf("handle alerts: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcomes": outcomeCounts(summary)})
}

// webhookAuthorized compares SHA-256 digests, so the comparison takes the
// same time whatever the length of the token presented.
func (s *server) webhookAuthorized(r *http.Request) bool {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(digest[:], s.webhookDigest[:]) == 1
}

// outcomeCounts lists every outcome, so a client reads a count without
// checking that the key exists.
func outcomeCounts(summary remediation.Summary) map[string]int {
	counts := make(map[string]int, len(summary.Outcomes))
	for _, o := range remediation.Outcomes() {
		counts[string(o)] = 0
	}
	for o, n := range summary.Outcomes {
		counts[string(o)] = n
	}
	return counts
}

func (s *server) listRemediations(w http.ResponseWriter, r *http.Request) {
	limit, err := parseRemediationLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	records, err := s.remediationLog.ListRemediations(r.Context(), limit)
	if err != nil {
		s.writeError(w, fmt.Errorf("list remediations: %w", err))
		return
	}
	out := make([]remediationJSON, 0, len(records))
	for _, rec := range records {
		out = append(out, remediationJSON{
			NodeID: rec.NodeID, AlertName: rec.AlertName, Fingerprint: rec.Fingerprint,
			Action: string(rec.Action), Outcome: string(rec.Outcome), At: rec.At.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"remediations": out})
}

func parseRemediationLimit(raw string) (int, error) {
	if raw == "" {
		return defaultRemediationLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxRemediationLimit {
		return 0, fmt.Errorf("%w: limit must be an integer from 1 to %d", errBadReq, maxRemediationLimit)
	}
	return limit, nil
}
