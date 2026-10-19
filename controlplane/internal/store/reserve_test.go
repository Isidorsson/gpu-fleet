package store

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
	"gpu-fleet/controlplane/internal/reservation"
)

var reservationIDPattern = regexp.MustCompile(`^res-[0-9a-f]{12}$`)

func TestCreateReservationBooksTheNodesThePlacerChose(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 6)
	spy := &spyPlacer{inner: takeFirst}
	req := request(3, placement.PolicySamePod, at(time.Hour), at(2*time.Hour))

	got, err := p.CreateReservation(t.Context(), req, spy.place)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !reservationIDPattern.MatchString(got.ID) {
		t.Errorf("id = %q, want res- and 12 hex characters", got.ID)
	}
	if got.Tenant != "acme" || got.Count != 3 || got.Policy != placement.PolicySamePod ||
		got.Status != reservation.StatusScheduled {
		t.Errorf("reservation = %+v", got)
	}
	if !got.StartsAt.Equal(req.StartsAt) || !got.EndsAt.Equal(req.EndsAt) || got.CreatedAt.IsZero() {
		t.Errorf("times = %v..%v created %v", got.StartsAt, got.EndsAt, got.CreatedAt)
	}
	wantIDs(t, "nodes", got.NodeIDs, "n01", "n02", "n03")
	wantIDs(t, "candidates offered", spy.lastSeen(), "n01", "n02", "n03", "n04", "n05", "n06")

	stored, err := p.GetReservation(t.Context(), got.ID)
	if err != nil || !reflect.DeepEqual(stored, got) {
		t.Fatalf("get = %+v, %v; want %+v", stored, err, got)
	}
}

func TestCreateReservationSkipsNodesBookedForTheWindow(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 6)
	window := func(req placement.Request, start, end time.Duration) reservation.Request {
		return request(req.Count, req.Policy, at(start), at(end))
	}
	three := placement.Request{Count: 3, Policy: placement.PolicyAny}

	first, err := p.CreateReservation(t.Context(), window(three, time.Hour, 2*time.Hour), takeFirst)
	second, err2 := p.CreateReservation(t.Context(), window(three, time.Hour, 2*time.Hour), takeFirst)
	if err != nil || err2 != nil {
		t.Fatalf("create: %v, %v", err, err2)
	}
	wantIDs(t, "first", first.NodeIDs, "n01", "n02", "n03")
	wantIDs(t, "second", second.NodeIDs, "n04", "n05", "n06")

	_, err = p.CreateReservation(t.Context(), window(three, time.Hour, 2*time.Hour), takeFirst)
	if !errors.Is(err, placement.ErrInsufficientCapacity) {
		t.Fatalf("third overlapping reservation: err = %v, want insufficient capacity", err)
	}

	later, err := p.CreateReservation(t.Context(), window(three, 2*time.Hour, 3*time.Hour), takeFirst)
	if err != nil {
		t.Fatalf("back-to-back reservation: %v", err)
	}
	wantIDs(t, "back-to-back", later.NodeIDs, "n01", "n02", "n03")
}

func TestCreateReservationReturnsPlacerErrorsUnchanged(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 2)
	want := &placement.CapacityError{Policy: placement.PolicySameRack, Requested: 4, Largest: 2}

	_, err := p.CreateReservation(t.Context(),
		request(4, placement.PolicySameRack, at(0), at(time.Hour)),
		func(placement.Request, []placement.Candidate) ([]string, error) { return nil, want })

	if err != error(want) {
		t.Fatalf("err = %v, want the placer's own *CapacityError", err)
	}
	if !errors.Is(err, placement.ErrInsufficientCapacity) {
		t.Fatalf("err = %v, want it to match ErrInsufficientCapacity", err)
	}
	if n := countRows(t, p, "reservations") + countRows(t, p, "reservation_nodes"); n != 0 {
		t.Fatalf("%d rows written for a failed placement, want 0", n)
	}
}

func TestCreateReservationRejectsBadPlacements(t *testing.T) {
	tests := map[string][]string{
		"too few nodes":      {"n01"},
		"too many nodes":     {"n01", "n02", "n03"},
		"a node twice":       {"n01", "n01"},
		"an unknown node":    {"n01", "n99"},
		"a quarantined node": {"n01", "n09"},
	}
	for name, ids := range tests {
		t.Run(name, func(t *testing.T) {
			p := openReservationStore(t)
			registerFleet(t, p, "n", 2)
			registerIn(t, p, "n09", node.StateQuarantined)

			_, err := p.CreateReservation(t.Context(),
				request(2, placement.PolicyAny, at(0), at(time.Hour)), fixedPlacer(ids...))

			if !errors.Is(err, ErrBadPlacement) {
				t.Fatalf("err = %v, want ErrBadPlacement", err)
			}
			if n := countRows(t, p, "reservations"); n != 0 {
				t.Fatalf("%d reservations written, want 0", n)
			}
		})
	}
}

func TestCreateReservationRejectsInvalidRequestsBeforePlacing(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 2)
	spy := &spyPlacer{inner: takeFirst}

	for name, req := range map[string]reservation.Request{
		"no nodes":      request(0, placement.PolicyAny, at(0), at(time.Hour)),
		"no tenant":     {Count: 1, Policy: placement.PolicyAny, StartsAt: at(0), EndsAt: at(time.Hour)},
		"backwards":     request(1, placement.PolicyAny, at(time.Hour), at(0)),
		"unknown":       request(1, "nearby", at(0), at(time.Hour)),
		"over 64 nodes": request(65, placement.PolicyAny, at(0), at(time.Hour)),
	} {
		if _, err := p.CreateReservation(t.Context(), req, spy.place); !errors.Is(err, reservation.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if n := spy.callCount(); n != 0 {
		t.Fatalf("placer called %d times for invalid requests, want 0", n)
	}
}

// rivalTakes returns a hook that makes another request win the race: after
// the placer picked its nodes, a rival books them, so the insert that follows
// loses to the exclusion constraint. It does so for the first wins calls.
func rivalTakes(t *testing.T, p *Postgres, wins int, start, end time.Time) func(int, []string) {
	return func(call int, ids []string) {
		if call > wins {
			return
		}
		rival := fmt.Sprintf("rival-%d", call)
		if err := insertReservationRows(t.Context(), p, rival, reservation.StatusScheduled, start, end, ids...); err != nil {
			t.Errorf("rival %d: %v", call, err)
		}
	}
}

func TestCreateReservationRetriesAfterLosingARace(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 8)
	start, end := at(time.Hour), at(2*time.Hour)
	spy := &spyPlacer{inner: takeFirst, before: rivalTakes(t, p, 2, start, end)}

	got, err := p.CreateReservation(t.Context(), request(2, placement.PolicyAny, start, end), spy.place)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	wantIDs(t, "nodes after two lost races", got.NodeIDs, "n05", "n06")
	if n := spy.callCount(); n != 3 {
		t.Fatalf("placer called %d times, want 3 (two lost races, one win)", n)
	}
	wantIDs(t, "candidates on the last attempt", spy.lastSeen(), "n05", "n06", "n07", "n08")
	if n := countRows(t, p, "reservations"); n != 3 {
		t.Fatalf("%d reservations, want the 2 rivals and ours", n)
	}
}

func TestCreateReservationGivesUpAfterMaxAttempts(t *testing.T) {
	p := openReservationStore(t)
	registerFleet(t, p, "n", 2*maxPlacementAttempts+2)
	start, end := at(time.Hour), at(2*time.Hour)
	spy := &spyPlacer{inner: takeFirst, before: rivalTakes(t, p, maxPlacementAttempts, start, end)}

	_, err := p.CreateReservation(t.Context(), request(2, placement.PolicyAny, start, end), spy.place)

	if !errors.Is(err, ErrPlacementContention) {
		t.Fatalf("err = %v, want ErrPlacementContention", err)
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != pgExclusionViolation {
		t.Errorf("err = %v, want the exclusion violation kept in the chain", err)
	}
	if n := spy.callCount(); n != maxPlacementAttempts {
		t.Fatalf("placer called %d times, want %d", n, maxPlacementAttempts)
	}
	if n := countRows(t, p, "reservations"); n != maxPlacementAttempts {
		t.Fatalf("%d reservations, want only the %d rivals", n, maxPlacementAttempts)
	}
}
