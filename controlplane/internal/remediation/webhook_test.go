package remediation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// alertmanagerPayload is a version 4 webhook as Alertmanager sends it, with
// the fields the control plane does not use.
const alertmanagerPayload = `{
  "receiver": "remediation",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "GPUFatalXid",
        "gpu": "3",
        "instance": "agent:9400",
        "job": "gpu-agent",
        "node_id": "node-003",
        "severity": "critical"
      },
      "annotations": {"summary": "GPU 3 on node-003 reported a fatal XID"},
      "startsAt": "2026-10-19T12:00:00.123Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus:9090/graph?g0.expr=gpu_xid_last",
      "fingerprint": "8f2a1c4d9b7e3a65"
    },
    {
      "status": "resolved",
      "labels": {"alertname": "GPUFatalXid", "node_id": "node-004"},
      "annotations": {},
      "startsAt": "2026-10-19T11:00:00Z",
      "endsAt": "2026-10-19T11:30:00Z",
      "generatorURL": "http://prometheus:9090/graph",
      "fingerprint": "11aa22bb33cc44dd"
    }
  ],
  "groupLabels": {"alertname": "GPUFatalXid", "node_id": "node-003"},
  "commonLabels": {"alertname": "GPUFatalXid", "severity": "critical"},
  "commonAnnotations": {},
  "externalURL": "http://alertmanager:9093",
  "version": "4",
  "groupKey": "{}/{alertname=\"GPUFatalXid\"}:{alertname=\"GPUFatalXid\", node_id=\"node-003\"}",
  "truncatedAlerts": 0
}`

func TestParseWebhookReadsAlertmanagerPayload(t *testing.T) {
	w, err := ParseWebhook(strings.NewReader(alertmanagerPayload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if w.Version != "4" || w.Status != "firing" || len(w.Alerts) != 2 {
		t.Fatalf("webhook = %+v", w)
	}
	a := w.Alerts[0]
	if a.Status != "firing" || a.Fingerprint != "8f2a1c4d9b7e3a65" ||
		a.Labels["alertname"] != "GPUFatalXid" || a.Labels["node_id"] != "node-003" || a.Labels["gpu"] != "3" ||
		a.Annotations["summary"] == "" {
		t.Fatalf("first alert = %+v", a)
	}
	if want := time.Date(2026, 10, 19, 12, 0, 0, 123_000_000, time.UTC); !a.StartsAt.Equal(want) {
		t.Fatalf("startsAt = %v, want %v", a.StartsAt, want)
	}
	if !a.EndsAt.IsZero() {
		t.Fatalf("endsAt = %v, want the zero time", a.EndsAt)
	}

	d, outcome := Decide(a)
	if d.NodeID != "node-003" || d.Action != ActionQuarantine || outcome != "" {
		t.Fatalf("Decide(first) = %+v, %q", d, outcome)
	}
	if _, outcome := Decide(w.Alerts[1]); outcome != OutcomeIgnored {
		t.Fatalf("Decide(resolved) outcome = %q, want ignored", outcome)
	}
}

func TestParseWebhookIgnoresUnknownFieldsAtEveryLevel(t *testing.T) {
	body := `{"version":"4","novel":{"a":1},"alerts":[{"status":"firing","labels":{},"extra":true}]}`
	w, err := ParseWebhook(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if len(w.Alerts) != 1 {
		t.Fatalf("alerts = %+v", w.Alerts)
	}
}

func TestParseWebhookRejectsUnsupportedVersion(t *testing.T) {
	for _, body := range []string{
		`{"version":"3","alerts":[]}`,
		`{"version":"5","alerts":[]}`,
		`{"version":"","alerts":[]}`,
		`{"alerts":[]}`,
	} {
		if _, err := ParseWebhook(strings.NewReader(body)); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("ParseWebhook(%s) = %v, want ErrUnsupportedVersion", body, err)
		}
	}
}

func TestParseWebhookRejectsMalformedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"empty":                "",
		"not json":             "version=4",
		"truncated":            `{"version":"4","alerts":[`,
		"wrong type":           `{"version":"4","alerts":"none"}`,
		"array":                `[]`,
		"trailing data":        `{"version":"4","alerts":[]} {"version":"4"}`,
		"trailing garbage":     `{"version":"4","alerts":[]}x`,
		"bad timestamp":        `{"version":"4","alerts":[{"startsAt":"yesterday"}]}`,
		"non-string label":     `{"version":"4","alerts":[{"labels":{"gpu":3}}]}`,
		"numeric version":      `{"version":4,"alerts":[]}`,
		"unterminated string":  `{"version":"4`,
		"only whitespace body": "  \n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseWebhook(strings.NewReader(body))
			if !errors.Is(err, ErrMalformedWebhook) {
				t.Fatalf("err = %v, want ErrMalformedWebhook", err)
			}
		})
	}
}

func TestParseWebhookAcceptsTrailingWhitespace(t *testing.T) {
	if _, err := ParseWebhook(strings.NewReader(`{"version":"4","alerts":[]}` + "\n\n")); err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
}

func TestParseWebhookSizeLimit(t *testing.T) {
	const prefix, suffix = `{"version":"4","alerts":[],"pad":"`, `"}`
	padded := func(total int) string {
		return prefix + strings.Repeat("x", total-len(prefix)-len(suffix)) + suffix
	}

	if _, err := ParseWebhook(strings.NewReader(padded(MaxWebhookBytes))); err != nil {
		t.Fatalf("body of exactly %d bytes: %v", MaxWebhookBytes, err)
	}
	_, err := ParseWebhook(strings.NewReader(padded(MaxWebhookBytes + 1)))
	if !errors.Is(err, ErrMalformedWebhook) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("body of %d bytes: err = %v, want ErrMalformedWebhook naming the limit", MaxWebhookBytes+1, err)
	}
}

func TestParseWebhookStopsReadingAtTheLimit(t *testing.T) {
	r := &countingReader{}
	_, err := ParseWebhook(r)
	if !errors.Is(err, ErrMalformedWebhook) {
		t.Fatalf("err = %v, want ErrMalformedWebhook", err)
	}
	if r.n > MaxWebhookBytes+1 {
		t.Fatalf("read %d bytes of an endless body, want at most %d", r.n, MaxWebhookBytes+1)
	}
}

type countingReader struct{ n int }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.n += len(p)
	return len(p), nil
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestParseWebhookWrapsReadErrors(t *testing.T) {
	boom := errors.New("connection reset")
	_, err := ParseWebhook(failingReader{boom})
	if !errors.Is(err, boom) || errors.Is(err, ErrMalformedWebhook) {
		t.Fatalf("err = %v, want the read error, not a malformed body", err)
	}
}
