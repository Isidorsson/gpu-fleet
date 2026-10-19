package bmcsim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser     = "admin"
	testPassword = "s3cret"
	resetPath    = "/redfish/v1/Systems/node-001/Actions/ComputerSystem.Reset"
)

type harness struct {
	handler http.Handler
	clock   *ManualClock
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg := Config{
		Nodes: 3, Seed: 1, BootLatency: testBootLatency, TaskDuration: testTaskDuration,
		User: testUser, Password: testPassword,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	clock := NewManualClock(testStart)
	handler, err := NewHandler(cfg, clock)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return &harness{handler: handler, clock: clock}
}

func (h *harness) do(method, path, body string) *httptest.ResponseRecorder {
	return h.doAs(testUser, testPassword, method, path, body)
}

func (h *harness) doAs(user, pass, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) mustDo(t *testing.T, wantStatus int, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := h.do(method, path, body)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, wantStatus, rec.Body)
	}
	return rec
}

func wantJSON(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var got, exp any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, rec.Body)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatalf("bad expectation: %v", err)
	}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("body = %s, want %s", rec.Body, want)
	}
}

func TestListSystems(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.mustDo(t, http.StatusOK, "GET", "/redfish/v1/Systems", "")

	wantJSON(t, rec, `{
		"Members": [
			{"@odata.id": "/redfish/v1/Systems/node-001"},
			{"@odata.id": "/redfish/v1/Systems/node-002"},
			{"@odata.id": "/redfish/v1/Systems/node-003"}
		],
		"Members@odata.count": 3
	}`)
}

func TestGetSystemShape(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.mustDo(t, http.StatusOK, "GET", "/redfish/v1/Systems/node-002", "")

	wantJSON(t, rec, `{
		"@odata.id": "/redfish/v1/Systems/node-002",
		"Id": "node-002",
		"PowerState": "Off",
		"Status": {"State": "Enabled", "Health": "OK"},
		"Boot": {"BootSourceOverrideTarget": "None", "BootSourceOverrideEnabled": "Disabled"}
	}`)
}

func TestPatchBootOverride(t *testing.T) {
	h := newHarness(t, nil)
	h.mustDo(t, http.StatusNoContent, "PATCH", "/redfish/v1/Systems/node-001",
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`)

	rec := h.mustDo(t, http.StatusOK, "GET", "/redfish/v1/Systems/node-001", "")
	var sys systemJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &sys); err != nil {
		t.Fatal(err)
	}
	if want := (bootJSON{Target: "Pxe", Enabled: "Once"}); sys.Boot != want {
		t.Fatalf("boot = %+v, want %+v", sys.Boot, want)
	}
}

func TestResetActions(t *testing.T) {
	h := newHarness(t, nil)
	powerState := func() string {
		rec := h.mustDo(t, http.StatusOK, "GET", "/redfish/v1/Systems/node-001", "")
		var sys systemJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &sys); err != nil {
			t.Fatal(err)
		}
		return sys.PowerState
	}

	steps := []struct{ resetType, want string }{
		{"On", "On"},
		{"On", "On"},
		{"ForceOff", "Off"},
		{"ForceRestart", "On"},
		{"ForceOff", "Off"},
	}
	for _, s := range steps {
		h.mustDo(t, http.StatusNoContent, "POST", resetPath, `{"ResetType": "`+s.resetType+`"}`)
		if got := powerState(); got != s.want {
			t.Fatalf("after %s: PowerState = %q, want %q", s.resetType, got, s.want)
		}
	}
}

func TestAgentTaskLifecycleOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	wantJSON(t, h.mustDo(t, http.StatusOK, "GET", "/agent/node-001/status", ""), `{"task": "", "phase": "idle"}`)

	h.mustDo(t, http.StatusNoContent, "PUT", "/agent/node-001/task", `{"task": "inspect"}`)
	h.mustDo(t, http.StatusNoContent, "PATCH", "/redfish/v1/Systems/node-001",
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`)
	h.mustDo(t, http.StatusNoContent, "POST", resetPath, `{"ResetType": "On"}`)

	wantJSON(t, h.do("GET", "/agent/node-001/status", ""), `{"task": "inspect", "phase": "running"}`)
	h.clock.Advance(testRunTime - time.Nanosecond)
	wantJSON(t, h.do("GET", "/agent/node-001/status", ""), `{"task": "inspect", "phase": "running"}`)
	h.clock.Advance(time.Nanosecond)
	wantJSON(t, h.do("GET", "/agent/node-001/status", ""), `{"task": "inspect", "phase": "succeeded"}`)

	wantJSON(t, h.do("GET", "/redfish/v1/Systems/node-001", ""), `{
		"@odata.id": "/redfish/v1/Systems/node-001",
		"Id": "node-001",
		"PowerState": "On",
		"Status": {"State": "Enabled", "Health": "OK"},
		"Boot": {"BootSourceOverrideTarget": "None", "BootSourceOverrideEnabled": "Disabled"}
	}`)
}

func TestFaultEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	h.mustDo(t, http.StatusNoContent, "POST", "/sim/systems/node-001/fault", "")

	health := func() string {
		var sys systemJSON
		if err := json.Unmarshal(h.do("GET", "/redfish/v1/Systems/node-001", "").Body.Bytes(), &sys); err != nil {
			t.Fatal(err)
		}
		return sys.Status.Health
	}
	if got := health(); got != "Critical" {
		t.Fatalf("health = %q, want Critical", got)
	}

	h.mustDo(t, http.StatusNoContent, "PUT", "/agent/node-001/task", `{"task": "wipe"}`)
	h.mustDo(t, http.StatusNoContent, "PATCH", "/redfish/v1/Systems/node-001",
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`)
	h.mustDo(t, http.StatusNoContent, "POST", resetPath, `{"ResetType": "On"}`)
	h.clock.Advance(testRunTime)
	wantJSON(t, h.do("GET", "/agent/node-001/status", ""), `{"task": "wipe", "phase": "failed"}`)

	h.mustDo(t, http.StatusNoContent, "DELETE", "/sim/systems/node-001/fault", "")
	if got := health(); got != "OK" {
		t.Fatalf("health = %q, want OK", got)
	}
}

func TestFaultFailsRunningTaskOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	h.mustDo(t, http.StatusNoContent, "PUT", "/agent/node-001/task", `{"task": "deploy"}`)
	h.mustDo(t, http.StatusNoContent, "PATCH", "/redfish/v1/Systems/node-001",
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`)
	h.mustDo(t, http.StatusNoContent, "POST", resetPath, `{"ResetType": "On"}`)

	h.mustDo(t, http.StatusNoContent, "POST", "/sim/systems/node-001/fault", "")
	h.mustDo(t, http.StatusNoContent, "DELETE", "/sim/systems/node-001/fault", "")
	h.clock.Advance(testRunTime)
	wantJSON(t, h.do("GET", "/agent/node-001/status", ""), `{"task": "deploy", "phase": "failed"}`)
}

type endpoint struct {
	method, path, body string
}

var systemEndpoints = []endpoint{
	{"GET", "/redfish/v1/Systems/{id}", ""},
	{"PATCH", "/redfish/v1/Systems/{id}", `{"Boot": {"BootSourceOverrideTarget": "Pxe"}}`},
	{"POST", "/redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset", `{"ResetType": "On"}`},
	{"PUT", "/agent/{id}/task", `{"task": "inspect"}`},
	{"GET", "/agent/{id}/status", ""},
}

var simEndpoints = []endpoint{
	{"POST", "/sim/systems/{id}/fault", ""},
	{"DELETE", "/sim/systems/{id}/fault", ""},
}

func (e endpoint) forID(id string) endpoint {
	e.path = strings.ReplaceAll(e.path, "{id}", id)
	return e
}

func allEndpoints() []endpoint {
	all := []endpoint{{"GET", "/redfish/v1/Systems", ""}}
	all = append(all, systemEndpoints...)
	return append(all, simEndpoints...)
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := newHarness(t, nil)
	creds := []struct{ name, user, pass string }{
		{"none", "", ""},
		{"wrong user", "root", testPassword},
		{"wrong password", testUser, "nope"},
		{"empty password", testUser, ""},
		{"password as prefix", testUser, testPassword[:len(testPassword)-1]},
	}
	for _, e := range allEndpoints() {
		e = e.forID("node-001")
		for _, c := range creds {
			t.Run(e.method+" "+e.path+" "+c.name, func(t *testing.T) {
				rec := h.doAs(c.user, c.pass, e.method, e.path, e.body)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", rec.Code)
				}
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("missing WWW-Authenticate header")
				}
			})
		}
	}
}

func TestUnknownPathRequiresAuth(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.doAs("", "", "GET", "/nope", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown path = %d, want 401", got)
	}
}

func TestUnknownSystemIsNotFound(t *testing.T) {
	h := newHarness(t, nil)
	for _, e := range append(append([]endpoint(nil), systemEndpoints...), simEndpoints...) {
		e = e.forID("node-999")
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			if got := h.do(e.method, e.path, e.body).Code; got != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", got)
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, nil)
	patch := "/redfish/v1/Systems/node-001"
	tests := []struct {
		name, method, path, body string
	}{
		{"bad ResetType", "POST", resetPath, `{"ResetType": "Reboot"}`},
		{"lowercase ResetType", "POST", resetPath, `{"ResetType": "on"}`},
		{"missing ResetType", "POST", resetPath, `{}`},
		{"unknown reset field", "POST", resetPath, `{"ResetType": "On", "Force": true}`},
		{"malformed reset JSON", "POST", resetPath, `{"ResetType": `},
		{"empty reset body", "POST", resetPath, ``},
		{"trailing data", "POST", resetPath, `{"ResetType": "On"} junk`},
		{"oversized body", "POST", resetPath, `{"ResetType": "` + strings.Repeat("x", maxBodyBytes) + `"}`},
		{"bad task", "PUT", "/agent/node-001/task", `{"task": "reboot"}`},
		{"missing task", "PUT", "/agent/node-001/task", `{}`},
		{"malformed task JSON", "PUT", "/agent/node-001/task", `not json`},
		{"unknown task field", "PUT", "/agent/node-001/task", `{"task": "wipe", "extra": 1}`},
		{"bad boot target", "PATCH", patch, `{"Boot": {"BootSourceOverrideTarget": "Usb"}}`},
		{"bad boot enabled", "PATCH", patch, `{"Boot": {"BootSourceOverrideEnabled": "Always"}}`},
		{"missing Boot", "PATCH", patch, `{}`},
		{"unknown boot field", "PATCH", patch, `{"Boot": {"Mode": "Uefi"}}`},
		{"malformed patch JSON", "PATCH", patch, `{"Boot": [`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.do(tc.method, tc.path, tc.body).Code; got != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", got)
			}
		})
	}
}

func TestBadRequestLeavesStateUntouched(t *testing.T) {
	h := newHarness(t, nil)
	h.mustDo(t, http.StatusBadRequest, "PATCH", "/redfish/v1/Systems/node-001",
		`{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Always"}}`)

	wantJSON(t, h.do("GET", "/redfish/v1/Systems/node-001", ""), `{
		"@odata.id": "/redfish/v1/Systems/node-001",
		"Id": "node-001",
		"PowerState": "Off",
		"Status": {"State": "Enabled", "Health": "OK"},
		"Boot": {"BootSourceOverrideTarget": "None", "BootSourceOverrideEnabled": "Disabled"}
	}`)
}

func TestBMCErrorRateOneAlwaysReturns503(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 1 })
	for _, e := range append([]endpoint{{"GET", "/redfish/v1/Systems", ""}}, systemEndpoints...) {
		e = e.forID("node-001")
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			for range 20 {
				if got := h.do(e.method, e.path, e.body).Code; got != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503", got)
				}
			}
		})
	}
}

func TestBMCErrorRateZeroNeverReturns503(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 0 })
	for range 200 {
		if got := h.do("GET", "/redfish/v1/Systems/node-001", "").Code; got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	}
}

func TestInjectedFailureDoesNotApplyTheRequest(t *testing.T) {
	s, err := newServer(Config{
		Nodes: 1, Seed: 1, BMCErrorRate: 1, User: testUser, Password: testPassword,
	}, NewManualClock(testStart))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{handler: s.routes()}
	h.mustDo(t, http.StatusServiceUnavailable, "POST", resetPath, `{"ResetType": "On"}`)

	m, _ := s.fleet.Machine("node-001")
	if got := m.Snapshot().Power; got != PowerOff {
		t.Fatalf("power = %q, want Off", got)
	}
}

func TestSimEndpointsExemptFromBMCErrorRate(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 1 })
	for _, e := range simEndpoints {
		e = e.forID("node-001")
		if got := h.do(e.method, e.path, e.body).Code; got != http.StatusNoContent {
			t.Fatalf("%s %s = %d, want 204", e.method, e.path, got)
		}
	}
}

func TestErrorPrecedence(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 1 })
	if got := h.doAs("", "", "GET", "/redfish/v1/Systems/node-001", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401 before 503", got)
	}
	if got := h.do("GET", "/redfish/v1/Systems/node-999", "").Code; got != http.StatusNotFound {
		t.Fatalf("unknown system = %d, want 404 before 503", got)
	}
}

func TestErrorInjectionIsSeededAndPartial(t *testing.T) {
	count := func() int {
		h := newHarness(t, func(c *Config) { c.BMCErrorRate = 0.5; c.Seed = 5 })
		n := 0
		for range 200 {
			if h.do("GET", "/redfish/v1/Systems/node-001", "").Code == http.StatusServiceUnavailable {
				n++
			}
		}
		return n
	}
	first := count()
	if first == 0 || first == 200 {
		t.Fatalf("503 count = %d of 200 at rate 0.5", first)
	}
	if second := count(); second != first {
		t.Fatalf("same seed gave %d then %d injected failures", first, second)
	}
}

func TestConcurrentRequestsAreRaceFree(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BMCErrorRate = 0.3 })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				h.do("PUT", "/agent/node-001/task", `{"task": "inspect"}`)
				h.do("POST", resetPath, `{"ResetType": "ForceRestart"}`)
				h.do("GET", "/agent/node-001/status", "")
				h.do("POST", "/sim/systems/node-001/fault", "")
				h.do("GET", "/redfish/v1/Systems/node-001", "")
				h.clock.Advance(time.Second)
			}
		})
	}
	wg.Wait()
}
