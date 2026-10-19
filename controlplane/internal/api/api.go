// Package api exposes the node lifecycle over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
	"gpu-fleet/controlplane/internal/store"
)

type Store interface {
	Register(ctx context.Context, n node.Node) error
	Get(ctx context.Context, id string) (node.Node, error)
	List(ctx context.Context, state node.State) ([]node.Node, error)
	ApplyEvent(ctx context.Context, id string, ev node.Event, tenant string) (node.Node, error)
	Events(ctx context.Context, id string) ([]store.EventRecord, error)

	FreeCandidates(ctx context.Context, start, end time.Time) ([]placement.Candidate, error)
	CreateReservation(ctx context.Context, req reservation.Request, place placement.Placer) (reservation.Reservation, error)
	GetReservation(ctx context.Context, id string) (reservation.Reservation, error)
	ListReservations(ctx context.Context, tenant string) ([]reservation.Reservation, error)
	CancelReservation(ctx context.Context, id string, now time.Time) (reservation.Reservation, error)
}

const maxBodyBytes = 1 << 20

var (
	validID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	errBadReq  = errors.New("bad request")
	errInvalid = errors.New("invalid request")
)

// Deps are what the handler needs. Store, Place and Summarize are required.
type Deps struct {
	Store Store
	// Place is handed to the store, which runs it inside the transaction that
	// creates a reservation.
	Place placement.Placer
	// Summarize condenses free candidates per pod for GET /v1/capacity.
	Summarize func([]placement.Candidate) []placement.PodCapacity
	// Metrics defaults to discarding everything.
	Metrics reservation.Metrics
	// Now defaults to time.Now.
	Now func() time.Time
	// Log defaults to slog.Default().
	Log *slog.Logger
}

type server struct {
	store     Store
	place     placement.Placer
	summarize func([]placement.Candidate) []placement.PodCapacity
	metrics   reservation.Metrics
	now       func() time.Time
	log       *slog.Logger
}

// NewHandler panics if a required dependency is missing: that is a wiring
// bug, not a runtime condition.
func NewHandler(d Deps) http.Handler {
	if d.Store == nil || d.Place == nil || d.Summarize == nil {
		panic("api: Deps.Store, Deps.Place and Deps.Summarize are required")
	}
	srv := &server{
		store: d.Store, place: d.Place, summarize: d.Summarize,
		metrics: d.Metrics, now: d.Now, log: d.Log,
	}
	if srv.metrics == nil {
		srv.metrics = reservation.NopMetrics{}
	}
	if srv.now == nil {
		srv.now = time.Now
	}
	if srv.log == nil {
		srv.log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/nodes", srv.registerNode)
	mux.HandleFunc("GET /v1/nodes", srv.listNodes)
	mux.HandleFunc("GET /v1/nodes/{id}", srv.getNode)
	mux.HandleFunc("POST /v1/nodes/{id}/events", srv.applyEvent)
	mux.HandleFunc("GET /v1/nodes/{id}/events", srv.listEvents)
	mux.HandleFunc("POST /v1/reservations", srv.createReservation)
	mux.HandleFunc("GET /v1/reservations", srv.listReservations)
	mux.HandleFunc("GET /v1/reservations/{id}", srv.getReservation)
	mux.HandleFunc("DELETE /v1/reservations/{id}", srv.cancelReservation)
	mux.HandleFunc("GET /v1/capacity", srv.capacity)
	return mux
}

type nodeJSON struct {
	ID     string `json:"id"`
	Pod    string `json:"pod"`
	Rack   string `json:"rack"`
	BMC    string `json:"bmc"`
	State  string `json:"state"`
	Tenant string `json:"tenant,omitempty"`
}

type eventJSON struct {
	From   string    `json:"from"`
	Event  string    `json:"event"`
	To     string    `json:"to"`
	Tenant string    `json:"tenant,omitempty"`
	At     time.Time `json:"at"`
}

func (s *server) registerNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Pod  string `json:"pod"`
		Rack string `json:"rack"`
		BMC  string `json:"bmc"`
	}
	if err := decode(w, r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	if !validID.MatchString(req.ID) || req.Pod == "" || req.Rack == "" {
		s.writeError(w, fmt.Errorf("%w: id must match %s, and pod and rack are required", errInvalid, validID))
		return
	}
	if !validBMC(req.BMC) {
		s.writeError(w, fmt.Errorf("%w: bmc must be an absolute http or https URL with a host, no credentials, query or fragment", errInvalid))
		return
	}
	n := node.Register(node.Spec{ID: req.ID, Pod: req.Pod, Rack: req.Rack, BMC: req.BMC})
	if err := s.store.Register(r.Context(), n); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toNodeJSON(n))
}

// validBMC rejects credentials because the URL is echoed back by the API;
// the BMC's login belongs in the control plane's own configuration.
func validBMC(raw string) bool {
	if strings.ContainsAny(raw, "?#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func (s *server) listNodes(w http.ResponseWriter, r *http.Request) {
	var state node.State
	if raw := r.URL.Query().Get("state"); raw != "" {
		parsed, err := node.ParseState(raw)
		if err != nil {
			s.writeError(w, fmt.Errorf("%w: %w", errBadReq, err))
			return
		}
		state = parsed
	}
	nodes, err := s.store.List(r.Context(), state)
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]nodeJSON, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toNodeJSON(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

func (s *server) getNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeJSON(n))
}

func (s *server) applyEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Event  string `json:"event"`
		Tenant string `json:"tenant"`
	}
	if err := decode(w, r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	ev, err := node.ParseEvent(req.Event)
	if err != nil {
		s.writeError(w, fmt.Errorf("%w: %w", errInvalid, err))
		return
	}
	n, err := s.store.ApplyEvent(r.Context(), r.PathValue("id"), ev, req.Tenant)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeJSON(n))
}

func (s *server) listEvents(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.Events(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]eventJSON, 0, len(records))
	for _, e := range records {
		out = append(out, eventJSON{
			From: string(e.From), Event: string(e.Event), To: string(e.To),
			Tenant: e.Tenant, At: e.At,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *server) writeError(w http.ResponseWriter, err error) {
	status := statusFor(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "err", err)
		msg = "internal error"
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, errBadReq):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound), errors.Is(err, reservation.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, node.ErrInvalidTransition),
		errors.Is(err, reservation.ErrNotCancellable), errors.Is(err, placement.ErrInsufficientCapacity):
		return http.StatusConflict
	case errors.Is(err, errInvalid), errors.Is(err, node.ErrTenantRequired), errors.Is(err, node.ErrUnexpectedTenant),
		errors.Is(err, reservation.ErrInvalid), errors.Is(err, placement.ErrInvalidRequest):
		return http.StatusUnprocessableEntity
	case errors.Is(err, store.ErrPlacementContention):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: malformed JSON body: %w", errBadReq, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers are sent; nothing useful left to do on failure
}

func toNodeJSON(n node.Node) nodeJSON {
	return nodeJSON{ID: n.ID, Pod: n.Pod, Rack: n.Rack, BMC: n.BMC, State: string(n.State), Tenant: n.Tenant}
}
