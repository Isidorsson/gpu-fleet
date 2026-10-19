package remediation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type storeCall struct {
	decision Decision
	limit    Limit
	now      time.Time
}

// fakeStore answers from outcomes by node ID (OutcomeQuarantined when absent)
// and fails for the nodes in failing.
type fakeStore struct {
	outcomes map[string]Outcome
	failing  map[string]error
	calls    []storeCall
}

func (s *fakeStore) ApplyRemediation(_ context.Context, d Decision, limit Limit, now time.Time) (Outcome, error) {
	s.calls = append(s.calls, storeCall{d, limit, now})
	if err := s.failing[d.NodeID]; err != nil {
		return "", err
	}
	if o, ok := s.outcomes[d.NodeID]; ok {
		return o, nil
	}
	return OutcomeQuarantined, nil
}

type spyMetrics struct{ outcomes []Outcome }

func (m *spyMetrics) Remediated(o Outcome) { m.outcomes = append(m.outcomes, o) }

func xidAlert(nodeID string) Alert {
	return Alert{
		Status:      StatusFiring,
		Labels:      map[string]string{LabelAlertName: "GPUFatalXid", LabelNodeID: nodeID},
		Fingerprint: "fp-" + nodeID,
	}
}

func webhook(alerts ...Alert) Webhook {
	return Webhook{Version: WebhookVersion, Status: StatusFiring, Alerts: alerts}
}

func newTestService(t *testing.T, cfg Config, store *fakeStore, m *spyMetrics, log *slog.Logger) *Service {
	t.Helper()
	s, err := New(cfg, Deps{Store: store, Clock: &fakeClock{now: testNow}, Metrics: m, Log: log})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestHandleCountsEveryOutcomeAndReportsEachAlert(t *testing.T) {
	store := &fakeStore{outcomes: map[string]Outcome{
		"node-002": OutcomeRateLimited,
		"node-003": OutcomeAlreadyQuarantined,
		"node-004": OutcomeUnknownNode,
		"node-005": OutcomeNotApplicable,
	}}
	m := &spyMetrics{}
	s := newTestService(t, Config{}, store, m, nil)

	resolved := xidAlert("node-001")
	resolved.Status = "resolved"
	otherRule := xidAlert("node-001")
	otherRule.Labels[LabelAlertName] = "GPUHot"
	noNode := Alert{Status: StatusFiring, Labels: map[string]string{LabelAlertName: "GPUFatalXid"}}

	sum, err := s.Handle(t.Context(), webhook(
		xidAlert("node-001"), xidAlert("node-002"), xidAlert("node-003"), xidAlert("node-004"), xidAlert("node-005"),
		resolved, otherRule, noNode, xidAlert("node-006"),
	))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	wantSummary := map[Outcome]int{
		OutcomeQuarantined: 2, OutcomeRateLimited: 1, OutcomeAlreadyQuarantined: 1,
		OutcomeUnknownNode: 1, OutcomeNotApplicable: 1, OutcomeIgnored: 3,
	}
	if !maps.Equal(sum.Outcomes, wantSummary) {
		t.Fatalf("summary = %v, want %v", sum.Outcomes, wantSummary)
	}
	wantMetrics := []Outcome{
		OutcomeQuarantined, OutcomeRateLimited, OutcomeAlreadyQuarantined, OutcomeUnknownNode, OutcomeNotApplicable,
		OutcomeIgnored, OutcomeIgnored, OutcomeIgnored, OutcomeQuarantined,
	}
	if !slices.Equal(m.outcomes, wantMetrics) {
		t.Fatalf("metrics = %v, want %v", m.outcomes, wantMetrics)
	}
	if len(store.calls) != 6 {
		t.Fatalf("store called %d times, want 6 (ignored alerts never reach it)", len(store.calls))
	}
}

func TestHandlePassesDecisionLimitAndClockToStore(t *testing.T) {
	store := &fakeStore{}
	limit := Limit{Max: 2, Window: time.Hour}
	s := newTestService(t, Config{Limit: limit}, store, &spyMetrics{}, nil)

	if _, err := s.Handle(t.Context(), webhook(xidAlert("node-003"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := storeCall{
		decision: Decision{NodeID: "node-003", AlertName: "GPUFatalXid", Fingerprint: "fp-node-003", Action: ActionQuarantine},
		limit:    limit,
		now:      testNow,
	}
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Fatalf("store calls = %+v, want [%+v]", store.calls, want)
	}
}

func TestHandleUsesDefaultLimitWhenConfigIsZero(t *testing.T) {
	store := &fakeStore{}
	s := newTestService(t, Config{}, store, &spyMetrics{}, nil)

	if _, err := s.Handle(t.Context(), webhook(xidAlert("node-001"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.calls[0].limit; got != DefaultLimit {
		t.Fatalf("limit = %+v, want %+v", got, DefaultLimit)
	}
}

func TestHandleReadsTheClockForEachAlert(t *testing.T) {
	store := &fakeStore{}
	clock := &fakeClock{now: testNow}
	s, err := New(Config{}, Deps{Store: store, Clock: clock})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Handle(t.Context(), webhook(xidAlert("node-001"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	clock.now = testNow.Add(time.Minute)
	if _, err := s.Handle(t.Context(), webhook(xidAlert("node-002"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.calls[0].now != testNow || store.calls[1].now != testNow.Add(time.Minute) {
		t.Fatalf("store times = %v, %v", store.calls[0].now, store.calls[1].now)
	}
}

func TestHandleRejectsUnsupportedVersion(t *testing.T) {
	for _, version := range []string{"", "3", "5", "4.0"} {
		t.Run("version "+version, func(t *testing.T) {
			store := &fakeStore{}
			m := &spyMetrics{}
			s := newTestService(t, Config{}, store, m, nil)

			w := webhook(xidAlert("node-001"))
			w.Version = version
			sum, err := s.Handle(t.Context(), w)
			if !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
			}
			if len(sum.Outcomes) != 0 || len(store.calls) != 0 || len(m.outcomes) != 0 {
				t.Fatalf("summary %v, %d store calls, %d metrics; want none", sum.Outcomes, len(store.calls), len(m.outcomes))
			}
		})
	}
}

func TestHandleStopsAtFirstStoreError(t *testing.T) {
	boom := errors.New("connection reset")
	store := &fakeStore{failing: map[string]error{"node-002": boom}}
	m := &spyMetrics{}
	s := newTestService(t, Config{}, store, m, nil)

	sum, err := s.Handle(t.Context(), webhook(xidAlert("node-001"), xidAlert("node-002"), xidAlert("node-003")))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the store error", err)
	}
	if !strings.Contains(err.Error(), "node-002") {
		t.Fatalf("err = %q, want it to name the node", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("store called %d times, want 2 (stop at the failure)", len(store.calls))
	}
	if !slices.Equal(m.outcomes, []Outcome{OutcomeQuarantined}) {
		t.Fatalf("metrics = %v, want only the alert handled before the failure", m.outcomes)
	}
	if !maps.Equal(sum.Outcomes, map[Outcome]int{OutcomeQuarantined: 1}) {
		t.Fatalf("summary = %v, want the one alert handled before the failure", sum.Outcomes)
	}
}

func TestHandleWithNoAlertsReturnsEmptySummary(t *testing.T) {
	s := newTestService(t, Config{}, &fakeStore{}, &spyMetrics{}, nil)
	sum, err := s.Handle(t.Context(), webhook())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sum.Outcomes == nil || len(sum.Outcomes) != 0 {
		t.Fatalf("outcomes = %#v, want empty non-nil map", sum.Outcomes)
	}
}

func TestHandleLogsQuarantinesAndRateLimits(t *testing.T) {
	store := &fakeStore{outcomes: map[string]Outcome{"node-002": OutcomeRateLimited, "node-003": OutcomeAlreadyQuarantined}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	s := newTestService(t, Config{}, store, &spyMetrics{}, log)

	if _, err := s.Handle(t.Context(), webhook(xidAlert("node-001"), xidAlert("node-002"), xidAlert("node-003"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "node_id=node-001") {
		t.Errorf("log lacks the quarantine of node-001:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "node_id=node-002") {
		t.Errorf("log lacks the rate limit for node-002:\n%s", out)
	}
	if strings.Contains(out, "node-003") {
		t.Errorf("log mentions node-003, whose outcome needs no log:\n%s", out)
	}
}

func TestNewValidates(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		deps Deps
		ok   bool
	}{
		{"zero config uses the default", Config{}, Deps{Store: &fakeStore{}}, true},
		{"custom limit", Config{Limit: Limit{Max: 1, Window: time.Second}}, Deps{Store: &fakeStore{}}, true},
		{"zero max", Config{Limit: Limit{Window: time.Minute}}, Deps{Store: &fakeStore{}}, false},
		{"zero window", Config{Limit: Limit{Max: 3}}, Deps{Store: &fakeStore{}}, false},
		{"negative window", Config{Limit: Limit{Max: 3, Window: -time.Second}}, Deps{Store: &fakeStore{}}, false},
		{"no store", Config{}, Deps{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(tt.cfg, tt.deps)
			if (err == nil) != tt.ok {
				t.Fatalf("New = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New err = %v, want ErrInvalidConfig", err)
			}
			if (s != nil) != tt.ok {
				t.Fatalf("service = %v, want non-nil=%v", s, tt.ok)
			}
		})
	}
}

func TestNewDefaultsClockMetricsAndLog(t *testing.T) {
	store := &fakeStore{}
	s, err := New(Config{}, Deps{Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := time.Now()
	if _, err := s.Handle(t.Context(), webhook(xidAlert("node-001"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.calls[0].now; got.Before(before) || got.After(time.Now()) {
		t.Fatalf("store time %v is not the wall clock", got)
	}
}
