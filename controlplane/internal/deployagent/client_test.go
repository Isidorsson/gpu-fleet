package deployagent

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
	testUser     = "agent-admin"
	testPassword = "s3cret-pass"
	testSystem   = "node-001"

	shortTimeout = 20 * time.Millisecond
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

type operation struct {
	name   string
	method string
	path   string
	run    func(ctx context.Context, c *Client) error
}

func operations() []operation {
	return []operation{
		{"SetTask", http.MethodPut, "/agent/" + testSystem + "/task", func(ctx context.Context, c *Client) error {
			return c.SetTask(ctx, testSystem, TaskInspect)
		}},
		{"Status", http.MethodGet, "/agent/" + testSystem + "/status", func(ctx context.Context, c *Client) error {
			_, err := c.Status(ctx, testSystem)
			return err
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

func TestBaseURLTrailingSlashDoesNotDoubleSlash(t *testing.T) {
	c, rec := newStub(t, http.StatusOK, `{"task": "", "phase": "idle"}`)
	c = New(c.baseURL+"/", testUser, testPassword, c.hc)
	if _, err := c.Status(t.Context(), testSystem); err != nil {
		t.Fatal(err)
	}
	assertRequest(t, rec.only(t), http.MethodGet, "/agent/node-001/status")
}

func TestSetTask(t *testing.T) {
	for _, task := range []Task{TaskInspect, TaskWipe, TaskDeploy} {
		t.Run(string(task), func(t *testing.T) {
			c, rec := newStub(t, http.StatusNoContent, "")

			if err := c.SetTask(t.Context(), testSystem, task); err != nil {
				t.Fatal(err)
			}
			got := rec.only(t)
			assertRequest(t, got, http.MethodPut, "/agent/node-001/task")
			assertJSONEqual(t, got.Body, fmt.Sprintf(`{"task": %q}`, task))
			if got.ContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got.ContentType)
			}
		})
	}
}

func TestSetTaskRejectsUnknownTaskWithoutRequest(t *testing.T) {
	c, rec := newStub(t, http.StatusNoContent, "")
	for _, task := range []Task{"", "format", "Inspect"} {
		if err := c.SetTask(t.Context(), testSystem, task); err == nil {
			t.Errorf("SetTask(%q) = nil, want error", task)
		}
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none: %+v", len(reqs), reqs)
	}
}

func TestStatusHappyPath(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Status
	}{
		{"running", `{"task": "inspect", "phase": "running"}`, Status{TaskInspect, PhaseRunning}},
		{"succeeded wipe", `{"task": "wipe", "phase": "succeeded"}`, Status{TaskWipe, PhaseSucceeded}},
		{"failed deploy", `{"task": "deploy", "phase": "failed"}`, Status{TaskDeploy, PhaseFailed}},
		{"idle empty task", `{"task": "", "phase": "idle"}`, Status{Phase: PhaseIdle}},
		{"idle missing task", `{"phase": "idle"}`, Status{Phase: PhaseIdle}},
		{"extra fields ignored", `{"task": "deploy", "phase": "running", "progress": 0.5}`, Status{TaskDeploy, PhaseRunning}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newStub(t, http.StatusOK, tc.body)

			got, err := c.Status(t.Context(), testSystem)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Status = %+v, want %+v", got, tc.want)
			}
			req := rec.only(t)
			assertRequest(t, req, http.MethodGet, "/agent/node-001/status")
			if req.Body != "" || req.ContentType != "" {
				t.Errorf("GET sent body %q with content type %q", req.Body, req.ContentType)
			}
		})
	}
}

func TestStatusRejectsUnknownValues(t *testing.T) {
	cases := map[string]string{
		"unknown phase": `{"task": "inspect", "phase": "paused"}`,
		"empty phase":   `{"task": "inspect", "phase": ""}`,
		"missing phase": `{"task": "inspect"}`,
		"uppercase":     `{"task": "inspect", "phase": "Running"}`,
		"unknown task":  `{"task": "format", "phase": "running"}`,
		"huge value":    `{"task": "` + strings.Repeat("x", 4096) + `", "phase": "running"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			_, err := c.Status(t.Context(), testSystem)
			if err == nil {
				t.Fatal("want error")
			}
			if errors.Is(err, ErrTransient) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnauthorized) {
				t.Errorf("parse failure must not map to a sentinel: %v", err)
			}
			if len(err.Error()) > 512 {
				t.Errorf("error echoes the whole remote value (%d bytes)", len(err.Error()))
			}
		})
	}
}

func TestStatusMalformedJSON(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"truncated":     `{"task": "inspect", `,
		"html":          `<html>busy</html>`,
		"wrong type":    `{"task": 5, "phase": "idle"}`,
		"array":         `[]`,
		"trailing junk": `{"task": "", "phase": "idle"} junk`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := newStub(t, http.StatusOK, body)
			if _, err := c.Status(t.Context(), testSystem); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	body := `{"task": "", "phase": "idle", "pad": "` + strings.Repeat("x", maxBodyBytes) + `"}`
	c, _ := newStub(t, http.StatusOK, body)

	_, err := c.Status(t.Context(), testSystem)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v, want errBodyTooLarge", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Errorf("oversized body is not transient: %v", err)
	}
}

func TestInvalidIDsRejectedWithoutRequest(t *testing.T) {
	c, rec := newStub(t, http.StatusNoContent, "")
	for _, id := range []string{"", ".", ".."} {
		if err := c.SetTask(t.Context(), id, TaskWipe); err == nil {
			t.Errorf("SetTask(%q) = nil error", id)
		}
		if _, err := c.Status(t.Context(), id); err == nil {
			t.Errorf("Status(%q) = nil error", id)
		}
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none: %+v", len(reqs), reqs)
	}
}

func TestIDsAreEscaped(t *testing.T) {
	const (
		id      = "rack 1/node#2?x%"
		escaped = "/agent/rack%201%2Fnode%232%3Fx%25"
	)
	c, rec := newStub(t, http.StatusOK, `{"task": "", "phase": "idle"}`)

	if err := c.SetTask(t.Context(), id, TaskDeploy); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	wantPaths := []string{escaped + "/task", escaped + "/status"}
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

	_, err := c.Status(t.Context(), testSystem)
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

	_, err := c.Status(ctx, testSystem)
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

	err := c.SetTask(ctx, testSystem, TaskWipe)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
	if reqs := rec.all(); len(reqs) != 0 {
		t.Errorf("sent %d requests, want none", len(reqs))
	}
}
