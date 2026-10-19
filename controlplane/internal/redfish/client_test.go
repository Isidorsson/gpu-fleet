package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser     = "bmc-admin"
	testPassword = "s3cret-pass"
	testSystem   = "node-001"

	shortTimeout = 20 * time.Millisecond

	docSystemJSON = `{
	  "@odata.id": "/redfish/v1/Systems/node-001",
	  "Id": "node-001",
	  "PowerState": "On",
	  "Status": {"State": "Enabled", "Health": "OK"},
	  "Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}
	}`
)

type recorded struct {
	Method      string
	Path        string
	RawQuery    string
	Body        string
	ContentType string
	User        string
	Password    string
	HasAuth     bool
}

type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (r *recorder) add(req recorded) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

func (r *recorder) only(t *testing.T) recorded {
	t.Helper()
	reqs := r.all()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1: %+v", len(reqs), reqs)
	}
	return reqs[0]
}

func newStub(t *testing.T, status int, body string) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("stub read body: %v", err)
		}
		user, password, ok := r.BasicAuth()
		rec.add(recorded{
			Method:      r.Method,
			Path:        r.URL.EscapedPath(),
			RawQuery:    r.URL.RawQuery,
			Body:        string(data),
			ContentType: r.Header.Get("Content-Type"),
			User:        user,
			Password:    password,
			HasAuth:     ok,
		})
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, testUser, testPassword, srv.Client()), rec
}

func assertRequest(t *testing.T, got recorded, method, path string) {
	t.Helper()
	if got.Method != method || got.Path != path {
		t.Errorf("request = %s %s, want %s %s", got.Method, got.Path, method, path)
	}
	if !got.HasAuth || got.User != testUser || got.Password != testPassword {
		t.Errorf("basic auth = %q/%q (present %v), want configured credentials", got.User, got.Password, got.HasAuth)
	}
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got body %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want body %q is not JSON: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func systemJSON(t *testing.T, id, power, health, target, enabled string) string {
	t.Helper()
	doc := map[string]any{
		"Id":         id,
		"PowerState": power,
		"Status":     map[string]string{"State": "Enabled", "Health": health},
		"Boot": map[string]string{
			"BootSourceOverrideTarget":  target,
			"BootSourceOverrideEnabled": enabled,
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type operation struct {
	name   string
	method string
	path   string
	run    func(ctx context.Context, c *Client) error
}

func operations() []operation {
	sysPath := "/redfish/v1/Systems/" + testSystem
	return []operation{
		{"Systems", http.MethodGet, "/redfish/v1/Systems", func(ctx context.Context, c *Client) error {
			_, err := c.Systems(ctx)
			return err
		}},
		{"System", http.MethodGet, sysPath, func(ctx context.Context, c *Client) error {
			_, err := c.System(ctx, testSystem)
			return err
		}},
		{"SetPXEBootOnce", http.MethodPatch, sysPath, func(ctx context.Context, c *Client) error {
			return c.SetPXEBootOnce(ctx, testSystem)
		}},
		{"Reset", http.MethodPost, sysPath + "/Actions/ComputerSystem.Reset", func(ctx context.Context, c *Client) error {
			return c.Reset(ctx, testSystem, ResetOn)
		}},
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("http://bmc.example:8443///", testUser, testPassword, nil)
	if c.hc == nil || c.hc.Timeout != defaultTimeout {
		t.Errorf("default client = %+v, want timeout %v", c.hc, defaultTimeout)
	}
	if c.baseURL != "http://bmc.example:8443" {
		t.Errorf("baseURL = %q, want trailing slashes trimmed", c.baseURL)
	}
}

func TestSystemsHappyPath(t *testing.T) {
	body := `{"Members": [
		{"@odata.id": "/redfish/v1/Systems/node-001"},
		{"@odata.id": "/redfish/v1/Systems/node-002"},
		{"@odata.id": "/redfish/v1/Systems/rack%201%2Fnode"}
	], "Members@odata.count": 3}`
	c, rec := newStub(t, http.StatusOK, body)

	ids, err := c.Systems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"node-001", "node-002", "rack 1/node"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %q, want %q", ids, want)
	}
	got := rec.only(t)
	assertRequest(t, got, http.MethodGet, "/redfish/v1/Systems")
	if got.Body != "" || got.ContentType != "" {
		t.Errorf("GET sent body %q with content type %q", got.Body, got.ContentType)
	}
}

func TestSystemsEmpty(t *testing.T) {
	c, _ := newStub(t, http.StatusOK, `{"Members": [], "Members@odata.count": 0}`)
	ids, err := c.Systems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %q, want none", ids)
	}
}

func TestBaseURLTrailingSlashDoesNotDoubleSlash(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, `{"Members": []}`)
	c = New(c.baseURL+"/", testUser, testPassword, c.hc)
	if _, err := c.Systems(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertRequest(t, rec.only(t), http.MethodGet, "/redfish/v1/Systems")
}

func TestSystemsRejectsBadMembers(t *testing.T) {
	bodies := map[string]string{
		"missing @odata.id": `{"Members": [{}]}`,
		"trailing slash":    `{"Members": [{"@odata.id": "/redfish/v1/Systems/"}]}`,
		"bad escape":        `{"Members": [{"@odata.id": "/redfish/v1/Systems/%zz"}]}`,
		"dot dot":           `{"Members": [{"@odata.id": "/redfish/v1/Systems/.."}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			if _, err := c.Systems(t.Context()); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestSystemHappyPath(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, docSystemJSON)

	got, err := c.System(t.Context(), testSystem)
	if err != nil {
		t.Fatal(err)
	}
	want := System{ID: testSystem, PowerState: PowerOn, Health: HealthOK, PXEOnce: true}
	if got != want {
		t.Errorf("System = %+v, want %+v", got, want)
	}
	assertRequest(t, rec.only(t), http.MethodGet, "/redfish/v1/Systems/node-001")
}

func TestSystemPXEOnceOnlyForPxeAndOnce(t *testing.T) {
	cases := []struct {
		target, enabled string
		want            bool
	}{
		{"Pxe", "Once", true},
		{"Pxe", "Disabled", false},
		{"None", "Once", false},
		{"None", "Disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.target+"/"+tc.enabled, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, systemJSON(t, testSystem, "Off", "Critical", tc.target, tc.enabled))
			got, err := c.System(t.Context(), testSystem)
			if err != nil {
				t.Fatal(err)
			}
			want := System{ID: testSystem, PowerState: PowerOff, Health: HealthCritical, PXEOnce: tc.want}
			if got != want {
				t.Errorf("System = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSystemRejectsUnknownValues(t *testing.T) {
	cases := map[string]string{
		"unknown power state":  systemJSON(t, testSystem, "Paused", "OK", "None", "Disabled"),
		"lowercase power":      systemJSON(t, testSystem, "on", "OK", "None", "Disabled"),
		"empty power state":    systemJSON(t, testSystem, "", "OK", "None", "Disabled"),
		"unknown health":       systemJSON(t, testSystem, "On", "Warning", "None", "Disabled"),
		"empty health":         systemJSON(t, testSystem, "On", "", "None", "Disabled"),
		"unknown boot target":  systemJSON(t, testSystem, "On", "OK", "Hdd", "Disabled"),
		"unknown boot enabled": systemJSON(t, testSystem, "On", "OK", "None", "Continuous"),
		"id mismatch":          systemJSON(t, "node-999", "On", "OK", "None", "Disabled"),
		"missing id":           `{"PowerState": "On", "Status": {"Health": "OK"}, "Boot": {"BootSourceOverrideTarget": "None", "BootSourceOverrideEnabled": "Disabled"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			_, err := c.System(t.Context(), testSystem)
			if err == nil {
				t.Fatal("want error")
			}
			if errors.Is(err, ErrTransient) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnauthorized) {
				t.Errorf("parse failure must not map to a sentinel: %v", err)
			}
		})
	}
}

func TestMalformedJSON(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"truncated":     `{"Members": [`,
		"html":          `<html>busy</html>`,
		"wrong type":    `{"Members": "x"}`,
		"trailing junk": `{"Members": []} junk`,
	}
	for name, body := range cases {
		t.Run("Systems/"+name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			if _, err := c.Systems(t.Context()); err == nil {
				t.Fatal("want error")
			}
		})
		t.Run("System/"+name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			if _, err := c.System(t.Context(), testSystem); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	body := `{"Members": [], "pad": "` + strings.Repeat("x", maxBodyBytes) + `"}`
	c, _ := newStub(t, http.StatusOK, body)

	_, err := c.Systems(t.Context())
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v, want errBodyTooLarge", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Errorf("oversized body is not transient: %v", err)
	}
}

func TestSetPXEBootOnce(t *testing.T) {
	c, rec := newStub(t, http.StatusNoContent, "")

	if err := c.SetPXEBootOnce(t.Context(), testSystem); err != nil {
		t.Fatal(err)
	}
	got := rec.only(t)
	assertRequest(t, got, http.MethodPatch, "/redfish/v1/Systems/node-001")
	assertJSONEqual(t, got.Body, `{"Boot": {"BootSourceOverrideTarget": "Pxe", "BootSourceOverrideEnabled": "Once"}}`)
	if got.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.ContentType)
	}
}

func TestReset(t *testing.T) {
	for _, rt := range []ResetType{ResetOn, ResetForceOff, ResetForceRestart} {
		t.Run(string(rt), func(t *testing.T) {
			c, rec := newStub(t, http.StatusNoContent, "")

			if err := c.Reset(t.Context(), testSystem, rt); err != nil {
				t.Fatal(err)
			}
			got := rec.only(t)
			assertRequest(t, got, http.MethodPost, "/redfish/v1/Systems/node-001/Actions/ComputerSystem.Reset")
			assertJSONEqual(t, got.Body, fmt.Sprintf(`{"ResetType": %q}`, rt))
			if got.ContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got.ContentType)
			}
		})
	}
}

func TestResetRejectsUnknownTypeWithoutRequest(t *testing.T) {
	c, rec := newStub(t, http.StatusNoContent, "")
	for _, rt := range []ResetType{"", "Nuke", "on"} {
		if err := c.Reset(t.Context(), testSystem, rt); err == nil {
			t.Errorf("Reset(%q) = nil, want error", rt)
		}
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none: %+v", len(reqs), reqs)
	}
}

func TestInvalidIDsRejectedWithoutRequest(t *testing.T) {
	c, rec := newStub(t, http.StatusNoContent, "")
	for _, id := range []string{"", ".", ".."} {
		if _, err := c.System(t.Context(), id); err == nil {
			t.Errorf("System(%q) = nil error", id)
		}
		if err := c.SetPXEBootOnce(t.Context(), id); err == nil {
			t.Errorf("SetPXEBootOnce(%q) = nil error", id)
		}
		if err := c.Reset(t.Context(), id, ResetOn); err == nil {
			t.Errorf("Reset(%q) = nil error", id)
		}
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none: %+v", len(reqs), reqs)
	}
}

func TestIDsAreEscaped(t *testing.T) {
	const (
		id      = "rack 1/node#2?x%"
		escaped = "/redfish/v1/Systems/rack%201%2Fnode%232%3Fx%25"
	)
	c, rec := newStub(t, http.StatusOK, systemJSON(t, id, "On", "OK", "None", "Disabled"))

	if _, err := c.System(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPXEBootOnce(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(t.Context(), id, ResetForceOff); err != nil {
		t.Fatal(err)
	}

	wantPaths := []string{escaped, escaped, escaped + "/Actions/ComputerSystem.Reset"}
	reqs := rec.all()
	if len(reqs) != len(wantPaths) {
		t.Fatalf("got %d requests, want %d", len(reqs), len(wantPaths))
	}
	for i, want := range wantPaths {
		if reqs[i].Path != want {
			t.Errorf("request %d path = %q, want %q", i, reqs[i].Path, want)
		}
		if reqs[i].RawQuery != "" {
			t.Errorf("request %d leaked id into query %q", i, reqs[i].RawQuery)
		}
	}
}

func TestStatusErrorMapping(t *testing.T) {
	sentinels := []error{ErrUnauthorized, ErrNotFound, ErrTransient}
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusBadGateway, ErrTransient},
		{http.StatusServiceUnavailable, ErrTransient},
		{http.StatusGatewayTimeout, ErrTransient},
		{http.StatusBadRequest, nil},
		{http.StatusForbidden, nil},
		{http.StatusInternalServerError, nil},
		{http.StatusNotImplemented, nil},
		{http.StatusMovedPermanently, nil},
	}
	for _, tc := range cases {
		for _, op := range operations() {
			t.Run(strconv.Itoa(tc.status)+"/"+op.name, func(t *testing.T) {
				c, _ := newStub(t, tc.status, "")

				err := op.run(t.Context(), c)
				if err == nil {
					t.Fatal("want error")
				}
				for _, s := range sentinels {
					if got := errors.Is(err, s); got != (s == tc.want) {
						t.Errorf("errors.Is(err, %v) = %v, want %v (err: %v)", s, got, !got, err)
					}
				}
				msg := err.Error()
				for _, part := range []string{op.method, op.path, strconv.Itoa(tc.status)} {
					if !strings.Contains(msg, part) {
						t.Errorf("error %q lacks %q", msg, part)
					}
				}
				if strings.Contains(msg, testPassword) {
					t.Errorf("error leaks the password: %q", msg)
				}
			})
		}
	}
}

func TestConnectionRefusedIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := New(srv.URL, testUser, testPassword, srv.Client())
	srv.Close()

	for _, op := range operations() {
		t.Run(op.name, func(t *testing.T) {
			err := op.run(t.Context(), c)
			if !errors.Is(err, ErrTransient) {
				t.Fatalf("err = %v, want ErrTransient", err)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

// newBlockingStub serves handlers that hold the request open until the
// client goes away. started receives one value per request that arrives.
func newBlockingStub(t *testing.T, hc *http.Client) (c *Client, started <-chan struct{}) {
	t.Helper()
	startedCh := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case startedCh <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	if hc == nil {
		hc = srv.Client()
	}
	return New(srv.URL, testUser, testPassword, hc), startedCh
}

func TestClientTimeoutIsTransientNotCallerDeadline(t *testing.T) {
	c, _ := newBlockingStub(t, &http.Client{Timeout: shortTimeout})

	err := c.Reset(t.Context(), testSystem, ResetOn)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("client timeout must not look like a caller ctx error: %v", err)
	}
}

func TestCallerCancelIsNotTransient(t *testing.T) {
	for _, op := range operations() {
		t.Run(op.name, func(t *testing.T) {
			c, started := newBlockingStub(t, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				<-started
				cancel()
			}()

			err := op.run(ctx, c)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if errors.Is(err, ErrTransient) {
				t.Errorf("caller cancel must not be ErrTransient: %v", err)
			}
		})
	}
}

func TestCallerDeadlineIsNotTransient(t *testing.T) {
	c, _ := newBlockingStub(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), shortTimeout)
	defer cancel()

	_, err := c.System(ctx, testSystem)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Errorf("caller deadline must not be ErrTransient: %v", err)
	}
}

func TestPreCanceledContextSendsNothing(t *testing.T) {
	c, rec := newStub(t, http.StatusNoContent, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := c.SetPXEBootOnce(ctx, testSystem)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none", len(reqs))
	}
}
