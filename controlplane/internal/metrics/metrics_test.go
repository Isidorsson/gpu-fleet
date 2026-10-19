package metrics

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"gpu-fleet/controlplane/internal/deployagent"
	"gpu-fleet/controlplane/internal/provisioner"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func scrape(t *testing.T, m *Metrics) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, string(body)
}

func recordOneOfEverything(m *Metrics) {
	p := m.Provisioner()
	p.TaskFinished(deployagent.TaskInspect, provisioner.OutcomeSucceeded, 0)
	p.BMCRequest(provisioner.OpStatus, provisioner.ResultOK)
	p.EventApplied("inspection_passed")
	p.LeaseLost()
}

func TestHandlerServesAllFamilies(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{})
	recordOneOfEverything(m)

	res, body := scrape(t, m)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain exposition", ct)
	}
	for _, want := range []string{
		"# TYPE fleet_nodes gauge",
		`fleet_nodes{state="racked"} 0`,
		`fleet_nodes{state="quarantined"} 0`,
		"# TYPE provisioner_tasks_total counter",
		"# TYPE provisioner_task_duration_seconds histogram",
		"# TYPE provisioner_bmc_requests_total counter",
		"# TYPE provisioner_events_total counter",
		"# TYPE provisioner_lease_lost_total counter",
		"# TYPE go_goroutines gauge",
		"# TYPE process_start_time_seconds gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

func TestHandlerKeepsServingOtherFamiliesWhenFleetQueryFails(t *testing.T) {
	m := newTestMetrics(t, &fakeCounter{err: errors.New("db down")})
	recordOneOfEverything(m)

	res, body := scrape(t, m)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with partial metrics", res.StatusCode)
	}
	if strings.Contains(body, "fleet_nodes") {
		t.Error("fleet_nodes served although its query failed")
	}
	for _, want := range []string{"provisioner_tasks_total", "provisioner_lease_lost_total", "go_goroutines"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

func TestNewDoesNotTouchTheDefaultRegistry(t *testing.T) {
	newTestMetrics(t, &fakeCounter{})
	newTestMetrics(t, &fakeCounter{})

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	for _, mf := range mfs {
		name := mf.GetName()
		if strings.HasPrefix(name, "fleet_") || strings.HasPrefix(name, "provisioner_") {
			t.Errorf("%s leaked into the default registry", name)
		}
	}
}
