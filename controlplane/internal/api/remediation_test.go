package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/remediation"
)

const (
	testWebhookToken = "remediation-test-token"
	validHookBody    = `{"version":"4","status":"firing","alerts":[]}`
)

// alertmanagerPayload is a version 4 webhook as Alertmanager sends it, with
// the fields the control plane ignores.
const alertmanagerPayload = `{
  "receiver": "remediation",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "GPUFatalXid", "node_id": "node-003", "gpu": "3", "severity": "critical",
        "job": "gpu-agent", "instance": "10.0.0.2:9400", "Hostname": "node-003",
        "UUID": "GPU-node-003-3", "modelName": "NVIDIA H100 80GB HBM3"
      },
      "annotations": {"summary": "GPU 3 on node-003 is unhealthy"},
      "startsAt": "2026-10-19T12:00:00.123Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus:9090/graph?g0.expr=gpu_agent_gpu_healthy+%3D%3D+0",
      "fingerprint": "8f2a1c9d3b7e4a60"
    }
  ],
  "groupLabels": {"alertname": "GPUFatalXid", "node_id": "node-003"},
  "commonLabels": {"alertname": "GPUFatalXid", "node_id": "node-003", "severity": "critical"},
  "commonAnnotations": {"summary": "GPU 3 on node-003 is unhealthy"},
  "externalURL": "http://alertmanager:9093",
  "version": "4",
  "groupKey": "{}/{alertname=\"GPUFatalXid\"}:{alertname=\"GPUFatalXid\", node_id=\"node-003\"}",
  "truncatedAlerts": 0
}`

type fakeRemediator struct {
	mu      sync.Mutex
	calls   []remediation.Webhook
	summary remediation.Summary
	err     error
}

func (f *fakeRemediator) Handle(_ context.Context, w remediation.Webhook) (remediation.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, w)
	return f.summary, f.err
}

func (f *fakeRemediator) received() []remediation.Webhook {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type fakeRemediationLog struct {
	mu      sync.Mutex
	records []remediation.Record
	maxes   []int
	err     error
}

func (f *fakeRemediationLog) ListRemediations(_ context.Context, max int) ([]remediation.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxes = append(f.maxes, max)
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.records[:min(max, len(f.records))]), nil
}

func (f *fakeRemediationLog) asked() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.maxes)
}

func withRemediator(r Remediator) func(*Deps) { return func(d *Deps) { d.Remediator = r } }

func withRemediationLog(l RemediationLog) func(*Deps) { return func(d *Deps) { d.RemediationLog = l } }

func bearer(token string) string { return "Bearer " + token }

// postAlerts sends body to POST /v1/alerts with the given Authorization
// header, which is left off when empty.
func postAlerts(t *testing.T, srv *httptest.Server, authorization, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/alerts", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/alerts: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, decodeBody(t, resp)
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestAlertsRejectMissingOrWrongCredentials(t *testing.T) {
	tests := map[string]string{
		"no header":             "",
		"basic scheme":          "Basic " + testWebhookToken,
		"unknown scheme":        "Token " + testWebhookToken,
		"token without scheme":  testWebhookToken,
		"scheme without token":  "Bearer",
		"scheme and space only": "Bearer ",
		"wrong token":           bearer("not-the-token"),
		"token with a suffix":   bearer(testWebhookToken + "x"),
		"token cut short":       bearer(testWebhookToken[:len(testWebhookToken)-1]),
		"token with a prefix":   bearer(" " + testWebhookToken),
		"very long token":       bearer(strings.Repeat("a", 64<<10)),
	}
	for name, authorization := range tests {
		t.Run(name, func(t *testing.T) {
			rem := &fakeRemediator{}
			srv := newTestServer(t, newFakeStore(), withRemediator(rem))

			resp, body := postAlerts(t, srv, authorization, validHookBody)

			if resp.StatusCode != http.StatusUnauthorized || body["error"] != "unauthorized" {
				t.Fatalf("status = %d %v, want 401 unauthorized", resp.StatusCode, body)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if calls := rem.received(); len(calls) != 0 {
				t.Errorf("Handle called %d times for an unauthorized request, want none", len(calls))
			}
		})
	}
}

func TestAlertsAuthIsCheckedBeforeTheBody(t *testing.T) {
	srv := newTestServer(t, newFakeStore())

	resp, _ := postAlerts(t, srv, "", "not json")

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 before the body is parsed", resp.StatusCode)
	}
}

func TestAlertsAcceptTheRightToken(t *testing.T) {
	for _, authorization := range []string{bearer(testWebhookToken), "bearer " + testWebhookToken} {
		rem := &fakeRemediator{}
		srv := newTestServer(t, newFakeStore(), withRemediator(rem))

		resp, body := postAlerts(t, srv, authorization, validHookBody)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%q: status = %d %v, want 200", authorization, resp.StatusCode, body)
		}
		if got := len(rem.received()); got != 1 {
			t.Errorf("%q: Handle called %d times, want 1", authorization, got)
		}
	}
}

func TestAlertsAcceptARealAlertmanagerPayload(t *testing.T) {
	rem := &fakeRemediator{}
	srv := newTestServer(t, newFakeStore(), withRemediator(rem))

	resp, body := postAlerts(t, srv, bearer(testWebhookToken), alertmanagerPayload)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %v, want 200", resp.StatusCode, body)
	}
	want := remediation.Webhook{
		Version: "4",
		Status:  "firing",
		Alerts: []remediation.Alert{{
			Status: "firing",
			Labels: map[string]string{
				"alertname": "GPUFatalXid", "node_id": "node-003", "gpu": "3", "severity": "critical",
				"job": "gpu-agent", "instance": "10.0.0.2:9400", "Hostname": "node-003",
				"UUID": "GPU-node-003-3", "modelName": "NVIDIA H100 80GB HBM3",
			},
			Annotations: map[string]string{"summary": "GPU 3 on node-003 is unhealthy"},
			StartsAt:    time.Date(2026, 10, 19, 12, 0, 0, 123_000_000, time.UTC),
			EndsAt:      time.Time{},
			Fingerprint: "8f2a1c9d3b7e4a60",
		}},
	}
	calls := rem.received()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("Handle got %+v, want one call with %+v", calls, want)
	}
}

func TestAlertsReturnEveryOutcomeWithZeroForTheAbsent(t *testing.T) {
	tests := map[string]struct {
		summary remediation.Summary
		want    map[string]any
	}{
		"some outcomes": {
			summary: remediation.Summary{Outcomes: map[remediation.Outcome]int{
				remediation.OutcomeQuarantined: 2, remediation.OutcomeRateLimited: 1,
			}},
			want: map[string]any{
				"quarantined": 2.0, "already_quarantined": 0.0, "not_applicable": 0.0,
				"unknown_node": 0.0, "rate_limited": 1.0, "ignored": 0.0,
			},
		},
		"no outcomes": {
			summary: remediation.Summary{},
			want: map[string]any{
				"quarantined": 0.0, "already_quarantined": 0.0, "not_applicable": 0.0,
				"unknown_node": 0.0, "rate_limited": 0.0, "ignored": 0.0,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t, newFakeStore(), withRemediator(&fakeRemediator{summary: tc.summary}))

			resp, body := postAlerts(t, srv, bearer(testWebhookToken), validHookBody)

			if resp.StatusCode != http.StatusOK || !reflect.DeepEqual(body, map[string]any{"outcomes": tc.want}) {
				t.Fatalf("response = %d %v, want 200 with outcomes %v", resp.StatusCode, body, tc.want)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestAlertsOutcomeKeysFollowTheOutcomeList(t *testing.T) {
	srv := newTestServer(t, newFakeStore())

	_, body := postAlerts(t, srv, bearer(testWebhookToken), validHookBody)

	outcomes, _ := body["outcomes"].(map[string]any)
	if len(outcomes) != len(remediation.Outcomes()) {
		t.Fatalf("outcomes = %v, want one key per outcome", outcomes)
	}
	for _, o := range remediation.Outcomes() {
		if _, ok := outcomes[string(o)]; !ok {
			t.Errorf("outcomes = %v, missing %q", outcomes, o)
		}
	}
}

func TestAlertsBodyLimitIsOneMiB(t *testing.T) {
	padded := func(n int) string {
		return `{"version":"4","status":"` + strings.Repeat("a", n) + `","alerts":[]}`
	}
	overhead := len(padded(0))
	tests := map[string]struct {
		body       string
		wantStatus int
		wantCalls  int
	}{
		"at the limit":   {padded(maxBodyBytes - overhead), http.StatusOK, 1},
		"over the limit": {padded(maxBodyBytes - overhead + 1), http.StatusBadRequest, 0},
		"far over":       {padded(4 * maxBodyBytes), http.StatusBadRequest, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rem := &fakeRemediator{}
			srv := newTestServer(t, newFakeStore(), withRemediator(rem))

			resp, body := postAlerts(t, srv, bearer(testWebhookToken), tc.body)

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d %v, want %d", resp.StatusCode, body, tc.wantStatus)
			}
			if got := len(rem.received()); got != tc.wantCalls {
				t.Errorf("Handle called %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestAlertsRejectMalformedJSON(t *testing.T) {
	tests := map[string]string{
		"empty body":        "",
		"not json":          "not json",
		"truncated":         `{"version":"4","alerts":[`,
		"array":             `[]`,
		"wrong field type":  `{"version":4}`,
		"alerts not a list": `{"version":"4","alerts":{}}`,
		"bad timestamp":     `{"version":"4","alerts":[{"startsAt":"yesterday"}]}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			rem := &fakeRemediator{}
			srv := newTestServer(t, newFakeStore(), withRemediator(rem))

			resp, body := postAlerts(t, srv, bearer(testWebhookToken), payload)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d %v, want 400", resp.StatusCode, body)
			}
			if calls := rem.received(); len(calls) != 0 {
				t.Errorf("Handle called %d times for a malformed body, want none", len(calls))
			}
		})
	}
}

func TestAlertsRejectUnsupportedVersion(t *testing.T) {
	tests := map[string]string{
		"older version":   `{"version":"3","status":"firing","alerts":[]}`,
		"newer version":   `{"version":"5","status":"firing","alerts":[]}`,
		"version missing": `{"status":"firing","alerts":[]}`,
		"empty version":   `{"version":"","status":"firing","alerts":[]}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			rem := &fakeRemediator{}
			srv := newTestServer(t, newFakeStore(), withRemediator(rem))

			resp, body := postAlerts(t, srv, bearer(testWebhookToken), payload)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d %v, want 400", resp.StatusCode, body)
			}
			if calls := rem.received(); len(calls) != 0 {
				t.Errorf("Handle called %d times for an unsupported version, want none", len(calls))
			}
		})
	}
}

func TestAlertsMapAnUnsupportedVersionFromHandleTo400(t *testing.T) {
	rem := &fakeRemediator{err: fmt.Errorf("handle: %w", remediation.ErrUnsupportedVersion)}
	srv := newTestServer(t, newFakeStore(), withRemediator(rem))

	resp, body := postAlerts(t, srv, bearer(testWebhookToken), validHookBody)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d %v, want 400", resp.StatusCode, body)
	}
}

func TestAlertsHandleFailureIsAGeneric500SoAlertmanagerRetries(t *testing.T) {
	cause := errors.New("apply remediation: connection reset: password=hunter2")
	srv := newTestServer(t, newFakeStore(), withRemediator(&fakeRemediator{err: cause}))

	resp, body := postAlerts(t, srv, bearer(testWebhookToken), validHookBody)

	if resp.StatusCode != http.StatusInternalServerError || !reflect.DeepEqual(body, map[string]any{"error": "internal error"}) {
		t.Fatalf("response = %d %v, want 500 with the generic body", resp.StatusCode, body)
	}
}

func TestAlertsNeverLogTheToken(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rem := &fakeRemediator{err: errors.New("db down")}
	srv := newTestServer(t, newFakeStore(), withRemediator(rem), func(d *Deps) { d.Log = log })
	const wrongToken = "guess-the-token"

	postAlerts(t, srv, bearer(wrongToken), validHookBody)
	postAlerts(t, srv, bearer(testWebhookToken), validHookBody)

	if logs.Len() == 0 {
		t.Fatal("nothing was logged, want the rejection and the failure")
	}
	for _, secret := range []string{wrongToken, testWebhookToken} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log contains the token %q:\n%s", secret, logs.String())
		}
	}
}

func TestListRemediationsShape(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	rlog := &fakeRemediationLog{records: []remediation.Record{
		{
			NodeID: "node-003", AlertName: "GPUFatalXid", Fingerprint: "8f2a", Action: remediation.ActionQuarantine,
			Outcome: remediation.OutcomeQuarantined, At: time.Date(2026, 10, 19, 14, 0, 5, 0, cest),
		},
		{
			NodeID: "node-009", AlertName: "GPUFatalXid", Fingerprint: "77c1", Action: remediation.ActionQuarantine,
			Outcome: remediation.OutcomeUnknownNode, At: time.Date(2026, 10, 19, 11, 59, 0, 250_000_000, time.UTC),
		},
	}}
	srv := newTestServer(t, newFakeStore(), withRemediationLog(rlog))

	status, body := do(t, srv, http.MethodGet, "/v1/remediations", "")

	want := map[string]any{"remediations": []any{
		map[string]any{
			"node_id": "node-003", "alertname": "GPUFatalXid", "fingerprint": "8f2a", "action": "quarantine",
			"outcome": "quarantined", "at": "2026-10-19T12:00:05Z",
		},
		map[string]any{
			"node_id": "node-009", "alertname": "GPUFatalXid", "fingerprint": "77c1", "action": "quarantine",
			"outcome": "unknown_node", "at": "2026-10-19T11:59:00.25Z",
		},
	}}
	if status != http.StatusOK || !reflect.DeepEqual(body, want) {
		t.Fatalf("list = %d %v, want 200 %v", status, body, want)
	}
}

func TestListRemediationsEmptyIsAnEmptyArray(t *testing.T) {
	srv := newTestServer(t, newFakeStore(), withRemediationLog(&fakeRemediationLog{}))

	status, body := do(t, srv, http.MethodGet, "/v1/remediations", "")

	if status != http.StatusOK || !reflect.DeepEqual(body, map[string]any{"remediations": []any{}}) {
		t.Fatalf("list = %d %v, want 200 with an empty array, not null", status, body)
	}
}

func TestListRemediationsLimit(t *testing.T) {
	tests := map[string]struct {
		query      string
		wantStatus int
		wantMax    int
	}{
		"default":         {"", http.StatusOK, 50},
		"empty value":     {"?limit=", http.StatusOK, 50},
		"smallest":        {"?limit=1", http.StatusOK, 1},
		"explicit":        {"?limit=120", http.StatusOK, 120},
		"largest":         {"?limit=500", http.StatusOK, 500},
		"zero":            {"?limit=0", http.StatusBadRequest, 0},
		"negative":        {"?limit=-5", http.StatusBadRequest, 0},
		"over the cap":    {"?limit=501", http.StatusBadRequest, 0},
		"far over":        {"?limit=99999999999999999999", http.StatusBadRequest, 0},
		"not a number":    {"?limit=abc", http.StatusBadRequest, 0},
		"fraction":        {"?limit=1.5", http.StatusBadRequest, 0},
		"trailing letter": {"?limit=10x", http.StatusBadRequest, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rlog := &fakeRemediationLog{}
			srv := newTestServer(t, newFakeStore(), withRemediationLog(rlog))

			status, body := do(t, srv, http.MethodGet, "/v1/remediations"+tc.query, "")

			if status != tc.wantStatus {
				t.Fatalf("status = %d %v, want %d", status, body, tc.wantStatus)
			}
			wantAsked := []int(nil)
			if tc.wantMax != 0 {
				wantAsked = []int{tc.wantMax}
			}
			if got := rlog.asked(); !slices.Equal(got, wantAsked) {
				t.Errorf("ListRemediations asked for %v, want %v", got, wantAsked)
			}
		})
	}
}

func TestListRemediationsFailureIsAGeneric500(t *testing.T) {
	rlog := &fakeRemediationLog{err: errors.New("connection reset: password=hunter2")}
	srv := newTestServer(t, newFakeStore(), withRemediationLog(rlog))

	status, body := do(t, srv, http.MethodGet, "/v1/remediations", "")

	if status != http.StatusInternalServerError || !reflect.DeepEqual(body, map[string]any{"error": "internal error"}) {
		t.Fatalf("list = %d %v, want 500 with the generic body", status, body)
	}
}

func TestRemediationListNeedsNoToken(t *testing.T) {
	srv := newTestServer(t, newFakeStore())

	status, _ := do(t, srv, http.MethodGet, "/v1/remediations", "")

	if status != http.StatusOK {
		t.Fatalf("list without a token = %d, want 200 like the other read endpoints", status)
	}
}
