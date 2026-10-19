// Package api exposes the node lifecycle over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/store"
)

type Store interface {
	Register(ctx context.Context, n node.Node) error
	Get(ctx context.Context, id string) (node.Node, error)
	List(ctx context.Context, state node.State) ([]node.Node, error)
	ApplyEvent(ctx context.Context, id string, ev node.Event, tenant string) (node.Node, error)
	Events(ctx context.Context, id string) ([]store.EventRecord, error)
}

const maxBodyBytes = 1 << 20

var (
	validID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	errBadReq  = errors.New("bad request")
	errInvalid = errors.New("invalid request")
)

type server struct {
	store Store
	log   *slog.Logger
}

func NewHandler(s Store, log *slog.Logger) http.Handler {
	srv := &server{store: s, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/nodes", srv.registerNode)
	mux.HandleFunc("GET /v1/nodes", srv.listNodes)
	mux.HandleFunc("GET /v1/nodes/{id}", srv.getNode)
	mux.HandleFunc("POST /v1/nodes/{id}/events", srv.applyEvent)
	mux.HandleFunc("GET /v1/nodes/{id}/events", srv.listEvents)
	return mux
}

type nodeJSON struct {
	ID     string `json:"id"`
	Rack   string `json:"rack"`
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
		Rack string `json:"rack"`
	}
	if err := decode(w, r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	if !validID.MatchString(req.ID) || req.Rack == "" {
		s.writeError(w, fmt.Errorf("%w: id must match %s and rack is required", errInvalid, validID))
		return
	}
	n := node.Register(req.ID, req.Rack)
	if err := s.store.Register(r.Context(), n); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toNodeJSON(n))
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
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, node.ErrInvalidTransition):
		return http.StatusConflict
	case errors.Is(err, errInvalid), errors.Is(err, node.ErrTenantRequired), errors.Is(err, node.ErrUnexpectedTenant):
		return http.StatusUnprocessableEntity
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
	return nodeJSON{ID: n.ID, Rack: n.Rack, State: string(n.State), Tenant: n.Tenant}
}
