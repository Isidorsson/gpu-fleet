package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

// defaultCapacityWindow is how long GET /v1/capacity looks ahead when the
// caller gives no ends_at.
const defaultCapacityWindow = time.Hour

type reservationJSON struct {
	ID        string    `json:"id"`
	Tenant    string    `json:"tenant"`
	Count     int       `json:"count"`
	Policy    string    `json:"policy"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Status    string    `json:"status"`
	Nodes     []string  `json:"nodes"`
	CreatedAt time.Time `json:"created_at"`
}

type podCapacityJSON struct {
	Pod         string `json:"pod"`
	Free        int    `json:"free"`
	LargestRack int    `json:"largest_rack"`
}

type capacityJSON struct {
	Pods      []podCapacityJSON `json:"pods"`
	TotalFree int               `json:"total_free"`
}

func (s *server) createReservation(w http.ResponseWriter, r *http.Request) {
	req, err := s.decodeReservationRequest(w, r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	res, err := s.store.CreateReservation(r.Context(), req, s.place)
	var capacity *placement.CapacityError
	if errors.As(err, &capacity) {
		s.metrics.PlacementFailed(req.Policy)
		writeJSON(w, http.StatusConflict, map[string]any{"error": capacity.Error(), "largest": capacity.Largest})
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toReservationJSON(res))
}

func (s *server) decodeReservationRequest(w http.ResponseWriter, r *http.Request) (reservation.Request, error) {
	var body struct {
		Tenant   string `json:"tenant"`
		Count    int    `json:"count"`
		Policy   string `json:"policy"`
		StartsAt string `json:"starts_at"`
		EndsAt   string `json:"ends_at"`
	}
	if err := decode(w, r, &body); err != nil {
		return reservation.Request{}, err
	}
	startsAt, err := parseOptionalTime("starts_at", body.StartsAt)
	if err != nil {
		return reservation.Request{}, err
	}
	endsAt, err := parseOptionalTime("ends_at", body.EndsAt)
	if err != nil {
		return reservation.Request{}, err
	}
	return reservation.Request{
		Tenant: body.Tenant, Count: body.Count, Policy: placement.Policy(body.Policy),
		StartsAt: startsAt, EndsAt: endsAt,
	}.Validate(s.now())
}

func (s *server) getReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.GetReservation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReservationJSON(res))
}

func (s *server) listReservations(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListReservations(r.Context(), r.URL.Query().Get("tenant"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]reservationJSON, 0, len(list))
	for _, res := range list {
		out = append(out, toReservationJSON(res))
	}
	writeJSON(w, http.StatusOK, map[string]any{"reservations": out})
}

func (s *server) cancelReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.CancelReservation(r.Context(), r.PathValue("id"), s.now())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReservationJSON(res))
}

func (s *server) capacity(w http.ResponseWriter, r *http.Request) {
	start, end, err := capacityWindow(r.URL.Query(), s.now())
	if err != nil {
		s.writeError(w, err)
		return
	}
	free, err := s.store.FreeCandidates(r.Context(), start, end)
	if err != nil {
		s.writeError(w, err)
		return
	}
	pods := s.summarize(free)
	out := capacityJSON{Pods: make([]podCapacityJSON, 0, len(pods)), TotalFree: len(free)}
	for _, p := range pods {
		out.Pods = append(out.Pods, podCapacityJSON{Pod: p.Pod, Free: p.Free, LargestRack: p.LargestRack})
	}
	writeJSON(w, http.StatusOK, out)
}

// capacityWindow reads the optional window. Like reservation.Request.Validate
// it clamps a start in the past to now; a missing end is an hour after the
// start.
func capacityWindow(q url.Values, now time.Time) (time.Time, time.Time, error) {
	start, err := parseOptionalTime("starts_at", q.Get("starts_at"))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseOptionalTime("ends_at", q.Get("ends_at"))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if start.Before(now) {
		start = now
	}
	if end.IsZero() {
		end = start.Add(defaultCapacityWindow)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: ends_at must be after starts_at", errBadReq)
	}
	return start, end, nil
}

// parseOptionalTime returns the zero time for an empty value.
func parseOptionalTime(field, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be an RFC 3339 timestamp such as 2026-01-02T15:04:05Z", errBadReq, field)
	}
	return t, nil
}

func toReservationJSON(r reservation.Reservation) reservationJSON {
	nodes := make([]string, len(r.NodeIDs))
	copy(nodes, r.NodeIDs)
	return reservationJSON{
		ID: r.ID, Tenant: r.Tenant, Count: r.Count, Policy: string(r.Policy),
		StartsAt: r.StartsAt.UTC(), EndsAt: r.EndsAt.UTC(), Status: string(r.Status),
		Nodes: nodes, CreatedAt: r.CreatedAt.UTC(),
	}
}
