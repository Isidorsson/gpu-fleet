package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
	"gpu-fleet/controlplane/internal/store"
)

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const createBody = `{"tenant":"acme","count":2,"policy":"same_rack","starts_at":"2026-03-02T00:00:00Z","ends_at":"2026-03-03T00:00:00Z"}`

var threeNodes = []placement.Candidate{
	{NodeID: "n1", Pod: "p1", Rack: "r1"},
	{NodeID: "n2", Pod: "p1", Rack: "r1"},
	{NodeID: "n3", Pod: "p2", Rack: "r2"},
}

// fakePlace takes the first Count candidates, or reports how many there are.
func fakePlace(req placement.Request, free []placement.Candidate) ([]string, error) {
	if len(free) < req.Count {
		return nil, &placement.CapacityError{Policy: req.Policy, Requested: req.Count, Largest: len(free)}
	}
	ids := make([]string, 0, req.Count)
	for _, c := range free[:req.Count] {
		ids = append(ids, c.NodeID)
	}
	return ids, nil
}

type fakeSummarizer struct {
	got []placement.Candidate
	out []placement.PodCapacity
}

func (f *fakeSummarizer) summarize(free []placement.Candidate) []placement.PodCapacity {
	f.got = free
	return f.out
}

type recordingMetrics struct {
	mu     sync.Mutex
	failed []placement.Policy
}

func (m *recordingMetrics) PlacementFailed(p placement.Policy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, p)
}

func (m *recordingMetrics) Degraded() {}

func (m *recordingMetrics) placementFailures() []placement.Policy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.failed)
}

type timeWindow struct{ start, end time.Time }

// fakeReservations is the reservation half of fakeStore. Every call fails
// with resErr when it is set; createErr applies to CreateReservation only.
type fakeReservations struct {
	rmu        sync.Mutex
	candidates []placement.Candidate
	byID       map[string]reservation.Reservation
	order      []string
	created    []reservation.Request
	windows    []timeWindow
	tenants    []string
	cancelled  []time.Time
	resErr     error
	createErr  error
}

func newFakeReservations() fakeReservations {
	return fakeReservations{byID: map[string]reservation.Reservation{}}
}

func (f *fakeReservations) seed(res reservation.Reservation) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.byID[res.ID] = res
	f.order = append(f.order, res.ID)
}

func (f *fakeReservations) FreeCandidates(_ context.Context, start, end time.Time) ([]placement.Candidate, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.windows = append(f.windows, timeWindow{start, end})
	if f.resErr != nil {
		return nil, f.resErr
	}
	return f.candidates, nil
}

func (f *fakeReservations) CreateReservation(_ context.Context, req reservation.Request, place placement.Placer) (reservation.Reservation, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.created = append(f.created, req)
	if err := firstErr(f.resErr, f.createErr); err != nil {
		return reservation.Reservation{}, err
	}
	ids, err := place(placement.Request{Count: req.Count, Policy: req.Policy}, f.candidates)
	if err != nil {
		return reservation.Reservation{}, err
	}
	res := reservation.Reservation{
		ID: fmt.Sprintf("res-%012d", len(f.order)+1), Tenant: req.Tenant, Count: req.Count,
		Policy: req.Policy, StartsAt: req.StartsAt, EndsAt: req.EndsAt,
		Status: reservation.StatusScheduled, NodeIDs: ids, CreatedAt: testNow,
	}
	f.byID[res.ID] = res
	f.order = append(f.order, res.ID)
	return res, nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeReservations) GetReservation(_ context.Context, id string) (reservation.Reservation, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if f.resErr != nil {
		return reservation.Reservation{}, f.resErr
	}
	res, ok := f.byID[id]
	if !ok {
		return reservation.Reservation{}, fmt.Errorf("get %s: %w", id, reservation.ErrNotFound)
	}
	return res, nil
}

func (f *fakeReservations) ListReservations(_ context.Context, tenant string) ([]reservation.Reservation, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.tenants = append(f.tenants, tenant)
	if f.resErr != nil {
		return nil, f.resErr
	}
	var out []reservation.Reservation
	for _, id := range f.order {
		if res := f.byID[id]; tenant == "" || res.Tenant == tenant {
			out = append(out, res)
		}
	}
	return out, nil
}

func (f *fakeReservations) CancelReservation(_ context.Context, id string, now time.Time) (reservation.Reservation, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.cancelled = append(f.cancelled, now)
	if f.resErr != nil {
		return reservation.Reservation{}, f.resErr
	}
	res, ok := f.byID[id]
	switch {
	case !ok:
		return reservation.Reservation{}, fmt.Errorf("cancel %s: %w", id, reservation.ErrNotFound)
	case res.Status == reservation.StatusEnded || res.Status == reservation.StatusCancelled:
		return res, fmt.Errorf("cancel %s: %w", id, reservation.ErrNotCancellable)
	}
	res.Status = reservation.StatusCancelled
	f.byID[id] = res
	return res, nil
}

func scheduled(id, tenant string, nodes ...string) reservation.Reservation {
	return reservation.Reservation{
		ID: id, Tenant: tenant, Count: len(nodes), Policy: placement.PolicySamePod,
		StartsAt: testNow.Add(24 * time.Hour), EndsAt: testNow.Add(48 * time.Hour),
		Status: reservation.StatusScheduled, NodeIDs: nodes, CreatedAt: testNow,
	}
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is %T, want a JSON object", v, v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is %T, want a JSON array (not null)", v, v)
	}
	return s
}

func assertEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func assertRFC3339(t *testing.T, what string, v any, want time.Time) {
	t.Helper()
	raw, ok := v.(string)
	if !ok {
		t.Fatalf("%s = %v, want an RFC 3339 string", what, v)
	}
	got, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("%s = %q, not RFC 3339: %v", what, raw, err)
	}
	if !got.Equal(want) {
		t.Errorf("%s = %s, want %s", what, raw, want.Format(time.RFC3339))
	}
	if want := want.UTC().Format(time.RFC3339); raw != want {
		t.Errorf("%s = %q, want normalized %q", what, raw, want)
	}
}

func TestCreateReservation(t *testing.T) {
	fs := newFakeStore()
	fs.candidates = threeNodes
	srv := newTestServer(t, fs)

	status, body := do(t, srv, http.MethodPost, "/v1/reservations", createBody)

	if status != http.StatusCreated {
		t.Fatalf("create = %d %v, want 201", status, body)
	}
	assertEqual(t, "id", body["id"], "res-000000000001")
	assertEqual(t, "tenant", body["tenant"], "acme")
	assertEqual(t, "count", body["count"], 2.0)
	assertEqual(t, "policy", body["policy"], "same_rack")
	assertEqual(t, "status", body["status"], "scheduled")
	assertRFC3339(t, "starts_at", body["starts_at"], time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	assertRFC3339(t, "ends_at", body["ends_at"], time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC))
	assertRFC3339(t, "created_at", body["created_at"], testNow)
	if nodes := asSlice(t, body["nodes"]); len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n2" {
		t.Errorf("nodes = %v, want the ids chosen by the placer", nodes)
	}
}

func TestCreateReservationPassesValidatedRequestToTheStore(t *testing.T) {
	fs := newFakeStore()
	fs.candidates = threeNodes
	srv := newTestServer(t, fs)

	do(t, srv, http.MethodPost, "/v1/reservations", createBody)

	want := reservation.Request{
		Tenant: "acme", Count: 2, Policy: placement.PolicySameRack,
		StartsAt: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), EndsAt: time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC),
	}
	if len(fs.created) != 1 || fs.created[0] != want {
		t.Fatalf("store received %v, want exactly %v", fs.created, want)
	}
}

func TestCreateReservationOutputIsUTCRFC3339(t *testing.T) {
	fs := newFakeStore()
	fs.candidates = threeNodes
	srv := newTestServer(t, fs)

	body := `{"tenant":"acme","count":1,"policy":"any","starts_at":"2026-03-02T01:00:00+01:00","ends_at":"2026-03-02T11:30:00.5-05:00"}`
	_, got := do(t, srv, http.MethodPost, "/v1/reservations", body)

	assertRFC3339(t, "starts_at", got["starts_at"], time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	if raw, _ := got["ends_at"].(string); raw != "2026-03-02T16:30:00.5Z" {
		t.Errorf("ends_at = %q, want the same instant in UTC", raw)
	}
}

func TestCreateReservationStartsNowWhenStartIsMissingOrPast(t *testing.T) {
	for name, startsAt := range map[string]string{
		"omitted": ``,
		"past":    `"starts_at":"2026-02-01T00:00:00Z",`,
	} {
		t.Run(name, func(t *testing.T) {
			fs := newFakeStore()
			fs.candidates = threeNodes
			srv := newTestServer(t, fs)

			body := `{"tenant":"acme","count":1,"policy":"any",` + startsAt + `"ends_at":"2026-03-01T18:00:00Z"}`
			status, got := do(t, srv, http.MethodPost, "/v1/reservations", body)

			if status != http.StatusCreated {
				t.Fatalf("create = %d %v, want 201", status, got)
			}
			assertEqual(t, "store starts_at", fs.created[0].StartsAt, testNow)
			assertRFC3339(t, "starts_at", got["starts_at"], testNow)
		})
	}
}

func TestCreateReservationInsufficientCapacityIs409WithLargest(t *testing.T) {
	for _, policy := range []placement.Policy{placement.PolicySameRack, placement.PolicySamePod, placement.PolicyAny} {
		t.Run(string(policy), func(t *testing.T) {
			fs := newFakeStore()
			fs.candidates = threeNodes
			metrics := &recordingMetrics{}
			srv := newTestServer(t, fs, func(d *Deps) { d.Metrics = metrics })

			body := fmt.Sprintf(`{"tenant":"acme","count":5,"policy":%q,"ends_at":"2026-03-02T00:00:00Z"}`, policy)
			status, got := do(t, srv, http.MethodPost, "/v1/reservations", body)

			if status != http.StatusConflict {
				t.Fatalf("create = %d %v, want 409", status, got)
			}
			assertEqual(t, "largest", got["largest"], 3.0)
			if msg, _ := got["error"].(string); msg == "" {
				t.Errorf("error message missing: %v", got)
			}
			if failures := metrics.placementFailures(); len(failures) != 1 || failures[0] != policy {
				t.Errorf("PlacementFailed calls = %v, want one for %s", failures, policy)
			}
		})
	}
}

func TestCreateReservationValidationIs422AndNeverReachesTheStore(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing tenant", `{"count":1,"policy":"any","ends_at":"2026-03-02T00:00:00Z"}`},
		{"zero count", `{"tenant":"acme","count":0,"policy":"any","ends_at":"2026-03-02T00:00:00Z"}`},
		{"too many nodes", `{"tenant":"acme","count":65,"policy":"any","ends_at":"2026-03-02T00:00:00Z"}`},
		{"unknown policy", `{"tenant":"acme","count":1,"policy":"closest","ends_at":"2026-03-02T00:00:00Z"}`},
		{"missing policy", `{"tenant":"acme","count":1,"ends_at":"2026-03-02T00:00:00Z"}`},
		{"missing end", `{"tenant":"acme","count":1,"policy":"any"}`},
		{"end before start", `{"tenant":"acme","count":1,"policy":"any","starts_at":"2026-03-05T00:00:00Z","ends_at":"2026-03-04T00:00:00Z"}`},
		{"end equals start", `{"tenant":"acme","count":1,"policy":"any","starts_at":"2026-03-05T00:00:00Z","ends_at":"2026-03-05T00:00:00Z"}`},
		{"end already passed", `{"tenant":"acme","count":1,"policy":"any","ends_at":"2026-02-01T00:00:00Z"}`},
		{"longer than the maximum", `{"tenant":"acme","count":1,"policy":"any","ends_at":"2027-03-01T00:00:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			metrics := &recordingMetrics{}
			srv := newTestServer(t, fs, func(d *Deps) { d.Metrics = metrics })

			status, got := do(t, srv, http.MethodPost, "/v1/reservations", tt.body)

			if status != http.StatusUnprocessableEntity {
				t.Fatalf("create = %d %v, want 422", status, got)
			}
			if len(fs.created) != 0 {
				t.Errorf("store received %v, want nothing", fs.created)
			}
			if len(metrics.placementFailures()) != 0 {
				t.Errorf("PlacementFailed called for an invalid request")
			}
		})
	}
}

func TestCreateReservationMalformedInputIs400(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `not json`},
		{"unknown field", `{"tenant":"acme","count":1,"policy":"any","ends_at":"2026-03-02T00:00:00Z","extra":1}`},
		{"count is a string", `{"tenant":"acme","count":"2","policy":"any","ends_at":"2026-03-02T00:00:00Z"}`},
		{"starts_at is not a timestamp", `{"tenant":"acme","count":1,"policy":"any","starts_at":"tomorrow","ends_at":"2026-03-02T00:00:00Z"}`},
		{"starts_at is a date", `{"tenant":"acme","count":1,"policy":"any","starts_at":"2026-03-02","ends_at":"2026-03-03T00:00:00Z"}`},
		{"ends_at without a zone", `{"tenant":"acme","count":1,"policy":"any","ends_at":"2026-03-02T00:00:00"}`},
		{"ends_at with a space", `{"tenant":"acme","count":1,"policy":"any","ends_at":"2026-03-02 00:00:00Z"}`},
		{"ends_at is a number", `{"tenant":"acme","count":1,"policy":"any","ends_at":1772409600}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			srv := newTestServer(t, fs)

			status, got := do(t, srv, http.MethodPost, "/v1/reservations", tt.body)

			if status != http.StatusBadRequest {
				t.Fatalf("create = %d %v, want 400", status, got)
			}
			if len(fs.created) != 0 {
				t.Errorf("store received %v, want nothing", fs.created)
			}
		})
	}
}

func TestCreateReservationStoreErrorsMapToStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"invalid", fmt.Errorf("store: %w", reservation.ErrInvalid), http.StatusUnprocessableEntity},
		{"capacity sentinel without detail", placement.ErrInsufficientCapacity, http.StatusConflict},
		{"lost every placement race", fmt.Errorf("create: %w", store.ErrPlacementContention), http.StatusServiceUnavailable},
		{"unexpected", errors.New("connection reset: password=hunter2"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			fs.createErr = tt.err
			metrics := &recordingMetrics{}
			srv := newTestServer(t, fs, func(d *Deps) { d.Metrics = metrics })

			status, got := do(t, srv, http.MethodPost, "/v1/reservations", createBody)

			assertEqual(t, "status", status, tt.want)
			if tt.want == http.StatusInternalServerError {
				assertEqual(t, "error body", got["error"], "internal error")
			}
			if len(metrics.placementFailures()) != 0 {
				t.Errorf("PlacementFailed called for a %s error", tt.name)
			}
		})
	}
}

func TestGetReservation(t *testing.T) {
	fs := newFakeStore()
	fs.seed(scheduled("res-a", "acme", "n1", "n2"))
	srv := newTestServer(t, fs)

	status, got := do(t, srv, http.MethodGet, "/v1/reservations/res-a", "")

	if status != http.StatusOK {
		t.Fatalf("get = %d %v, want 200", status, got)
	}
	assertEqual(t, "id", got["id"], "res-a")
	assertEqual(t, "policy", got["policy"], "same_pod")
	assertRFC3339(t, "starts_at", got["starts_at"], testNow.Add(24*time.Hour))
	if nodes := asSlice(t, got["nodes"]); len(nodes) != 2 {
		t.Errorf("nodes = %v, want 2", nodes)
	}
	if status, _ := do(t, srv, http.MethodGet, "/v1/reservations/res-missing", ""); status != http.StatusNotFound {
		t.Errorf("get unknown = %d, want 404", status)
	}
}

func TestListReservations(t *testing.T) {
	fs := newFakeStore()
	fs.seed(scheduled("res-a", "acme", "n1"))
	fs.seed(scheduled("res-b", "globex", "n2"))
	fs.seed(scheduled("res-c", "acme", "n3"))
	srv := newTestServer(t, fs)

	_, all := do(t, srv, http.MethodGet, "/v1/reservations", "")
	_, acme := do(t, srv, http.MethodGet, "/v1/reservations?tenant=acme", "")

	assertEqual(t, "all", len(asSlice(t, all["reservations"])), 3)
	mine := asSlice(t, acme["reservations"])
	if len(mine) != 2 || asMap(t, mine[0])["id"] != "res-a" || asMap(t, mine[1])["id"] != "res-c" {
		t.Errorf("acme reservations = %v, want res-a and res-c", mine)
	}
	if !slices.Equal(fs.tenants, []string{"", "acme"}) {
		t.Errorf("store tenant filters = %q, want [\"\" acme]", fs.tenants)
	}
}

func TestListReservationsIsAnEmptyArrayWhenThereAreNone(t *testing.T) {
	srv := newTestServer(t, newFakeStore())

	status, got := do(t, srv, http.MethodGet, "/v1/reservations?tenant=nobody", "")

	assertEqual(t, "status", status, http.StatusOK)
	assertEqual(t, "reservations", len(asSlice(t, got["reservations"])), 0)
}

func TestReservationWithoutNodesRendersAnEmptyArray(t *testing.T) {
	fs := newFakeStore()
	fs.seed(reservation.Reservation{ID: "res-a", Tenant: "acme", Status: reservation.StatusEnded})
	srv := newTestServer(t, fs)

	_, got := do(t, srv, http.MethodGet, "/v1/reservations/res-a", "")

	assertEqual(t, "nodes", len(asSlice(t, got["nodes"])), 0)
}

func TestCancelReservation(t *testing.T) {
	fs := newFakeStore()
	fs.seed(scheduled("res-a", "acme", "n1"))
	srv := newTestServer(t, fs)

	status, got := do(t, srv, http.MethodDelete, "/v1/reservations/res-a", "")

	if status != http.StatusOK {
		t.Fatalf("cancel = %d %v, want 200", status, got)
	}
	assertEqual(t, "status field", got["status"], "cancelled")
	if len(fs.cancelled) != 1 || !fs.cancelled[0].Equal(testNow) {
		t.Errorf("store cancelled at %v, want the handler's now %v", fs.cancelled, testNow)
	}
}

func TestCancelReservationErrors(t *testing.T) {
	fs := newFakeStore()
	fs.seed(scheduled("res-a", "acme", "n1"))
	ended := scheduled("res-ended", "acme", "n2")
	ended.Status = reservation.StatusEnded
	fs.seed(ended)
	srv := newTestServer(t, fs)

	if status, _ := do(t, srv, http.MethodDelete, "/v1/reservations/res-missing", ""); status != http.StatusNotFound {
		t.Errorf("cancel unknown = %d, want 404", status)
	}
	if status, _ := do(t, srv, http.MethodDelete, "/v1/reservations/res-ended", ""); status != http.StatusConflict {
		t.Errorf("cancel ended = %d, want 409", status)
	}
	do(t, srv, http.MethodDelete, "/v1/reservations/res-a", "")
	if status, _ := do(t, srv, http.MethodDelete, "/v1/reservations/res-a", ""); status != http.StatusConflict {
		t.Errorf("cancel twice = %d, want 409", status)
	}
}

func TestReservationEndpointsHideStoreFailures(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/reservations", createBody},
		{http.MethodGet, "/v1/reservations", ""},
		{http.MethodGet, "/v1/reservations/res-a", ""},
		{http.MethodDelete, "/v1/reservations/res-a", ""},
		{http.MethodGet, "/v1/capacity", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			fs := newFakeStore()
			fs.resErr = errors.New("connection reset: password=hunter2")
			srv := newTestServer(t, fs)

			status, got := do(t, srv, rt.method, rt.path, rt.body)

			if status != http.StatusInternalServerError || got["error"] != "internal error" {
				t.Fatalf("store failure = %d %v, want generic 500", status, got)
			}
		})
	}
}

func TestCapacity(t *testing.T) {
	fs := newFakeStore()
	fs.candidates = threeNodes
	summarizer := &fakeSummarizer{out: []placement.PodCapacity{
		{Pod: "p1", Free: 2, LargestRack: 2},
		{Pod: "p2", Free: 1, LargestRack: 1},
	}}
	srv := newTestServer(t, fs, func(d *Deps) { d.Summarize = summarizer.summarize })

	status, got := do(t, srv, http.MethodGet, "/v1/capacity", "")

	if status != http.StatusOK {
		t.Fatalf("capacity = %d %v, want 200", status, got)
	}
	assertEqual(t, "total_free", got["total_free"], 3.0)
	pods := asSlice(t, got["pods"])
	if len(pods) != 2 {
		t.Fatalf("pods = %v, want 2", pods)
	}
	first := asMap(t, pods[0])
	assertEqual(t, "first pod", first["pod"], "p1")
	assertEqual(t, "first free", first["free"], 2.0)
	assertEqual(t, "first largest_rack", first["largest_rack"], 2.0)
	assertEqual(t, "second pod", asMap(t, pods[1])["pod"], "p2")
	if !slices.Equal(summarizer.got, threeNodes) {
		t.Errorf("summarizer received %v, want the store's free candidates", summarizer.got)
	}
}

func TestCapacityWithNothingFreeIsEmpty(t *testing.T) {
	srv := newTestServer(t, newFakeStore())

	status, got := do(t, srv, http.MethodGet, "/v1/capacity", "")

	assertEqual(t, "status", status, http.StatusOK)
	assertEqual(t, "pods", len(asSlice(t, got["pods"])), 0)
	assertEqual(t, "total_free", got["total_free"], 0.0)
}

func TestCapacityWindow(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  timeWindow
	}{
		{"default is now to one hour", "", timeWindow{testNow, testNow.Add(time.Hour)}},
		{
			"explicit window", "?starts_at=2026-03-02T00:00:00Z&ends_at=2026-03-02T06:00:00Z",
			timeWindow{testNow.Add(12 * time.Hour), testNow.Add(18 * time.Hour)},
		},
		{
			"only a start runs for an hour", "?starts_at=2026-03-02T00:00:00Z",
			timeWindow{testNow.Add(12 * time.Hour), testNow.Add(13 * time.Hour)},
		},
		{"only an end starts now", "?ends_at=2026-03-01T18:00:00Z", timeWindow{testNow, testNow.Add(6 * time.Hour)}},
		{
			"a past start is clamped to now", "?starts_at=2026-02-01T00:00:00Z&ends_at=2026-03-01T13:00:00Z",
			timeWindow{testNow, testNow.Add(time.Hour)},
		},
		{
			"offsets are honored", "?starts_at=" + url.QueryEscape("2026-03-02T01:00:00+01:00") + "&ends_at=2026-03-02T06:00:00Z",
			timeWindow{testNow.Add(12 * time.Hour), testNow.Add(18 * time.Hour)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			srv := newTestServer(t, fs)

			status, got := do(t, srv, http.MethodGet, "/v1/capacity"+tt.query, "")

			if status != http.StatusOK {
				t.Fatalf("capacity = %d %v, want 200", status, got)
			}
			if len(fs.windows) != 1 || !fs.windows[0].start.Equal(tt.want.start) || !fs.windows[0].end.Equal(tt.want.end) {
				t.Errorf("store window = %v, want %v", fs.windows, tt.want)
			}
		})
	}
}

func TestCapacityBadWindowIs400(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"starts_at is not a timestamp", "?starts_at=tomorrow"},
		{"starts_at is a date", "?starts_at=2026-03-02"},
		{"ends_at is not a timestamp", "?ends_at=soon"},
		{"ends_at without a zone", "?ends_at=2026-03-02T00:00:00"},
		{"end equals start", "?starts_at=2026-03-02T00:00:00Z&ends_at=2026-03-02T00:00:00Z"},
		{"end before start", "?starts_at=2026-03-03T00:00:00Z&ends_at=2026-03-02T00:00:00Z"},
		{"end already passed", "?ends_at=2026-02-01T00:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			srv := newTestServer(t, fs)

			status, got := do(t, srv, http.MethodGet, "/v1/capacity"+tt.query, "")

			if status != http.StatusBadRequest {
				t.Fatalf("capacity = %d %v, want 400", status, got)
			}
			if len(fs.windows) != 0 {
				t.Errorf("store queried for %v, want no query", fs.windows)
			}
		})
	}
}

func TestNewHandlerRequiresItsDependencies(t *testing.T) {
	complete := testDeps(newFakeStore())
	tests := map[string]func(*Deps){
		"store":           func(d *Deps) { d.Store = nil },
		"place":           func(d *Deps) { d.Place = nil },
		"summarize":       func(d *Deps) { d.Summarize = nil },
		"remediator":      func(d *Deps) { d.Remediator = nil },
		"remediation log": func(d *Deps) { d.RemediationLog = nil },
		"webhook token":   func(d *Deps) { d.WebhookToken = "" },
	}
	for name, remove := range tests {
		t.Run(name, func(t *testing.T) {
			deps := complete
			remove(&deps)
			defer func() {
				if recover() == nil {
					t.Error("NewHandler did not panic")
				}
			}()
			NewHandler(deps)
		})
	}
}

func TestNewHandlerDefaultsMetricsClockAndLog(t *testing.T) {
	fs := newFakeStore()
	fs.candidates = threeNodes
	srv := newTestServer(t, fs, func(d *Deps) { d.Metrics, d.Now, d.Log = nil, nil, nil })

	body := fmt.Sprintf(`{"tenant":"acme","count":9,"policy":"any","ends_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	status, got := do(t, srv, http.MethodPost, "/v1/reservations", body)
	if status != http.StatusConflict {
		t.Fatalf("over-capacity create = %d %v, want 409 even without metrics", status, got)
	}

	before := time.Now()
	do(t, srv, http.MethodGet, "/v1/capacity", "")
	if len(fs.windows) != 1 || fs.windows[0].start.Before(before.Add(-time.Minute)) || fs.windows[0].start.After(time.Now()) {
		t.Errorf("default clock window = %v, want a start of about now", fs.windows)
	}
}
