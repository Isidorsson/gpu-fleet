package chaos

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

// Kind names the four things chaos does.
type Kind string

const (
	KindRepair  Kind = "repair"
	KindXID     Kind = "xid"
	KindImplant Kind = "implant"
	KindReserve Kind = "reserve"
)

// State is what chaos remembers between ticks. It is a value: the methods
// return a new State and never touch the receiver, so the planner stays pure.
//
//   - Last is when each kind of action was last attempted.
//   - XIDTimes are the recent xid attempts, for the rolling rate bound.
//   - Pending holds nodes chaos faulted (xid) or implanted that have not been
//     quarantined yet, and when.
//   - QuarantinedSince is when a node was first seen quarantined. The API
//     reports no timestamp on a node, so the age is observed, and restarts
//     the clock after a chaos restart.
//   - TenantsIssued numbers the tenants: the next one is tenant-<n+1>.
type State struct {
	Last             map[Kind]time.Time
	XIDTimes         []time.Time
	Pending          map[string]time.Time
	QuarantinedSince map[string]time.Time
	TenantsIssued    int
}

// Observe folds a snapshot in: it timestamps newly quarantined nodes, forgets
// nodes that left quarantine, drops pending entries that were quarantined
// (the snapshot counts them now) or are stale, and moves the tenant counter
// past any tenant-<n> already booked, so a restart does not reuse names.
func (s State) Observe(snap Snapshot, now time.Time) State {
	next := s.clone()
	quarantined := map[string]bool{}
	next.QuarantinedSince = map[string]time.Time{}
	for _, n := range snap.Nodes {
		if n.State != node.StateQuarantined {
			continue
		}
		quarantined[n.ID] = true
		since, seen := s.QuarantinedSince[n.ID]
		if !seen {
			since = now
		}
		next.QuarantinedSince[n.ID] = since
	}
	for id, at := range s.Pending {
		if quarantined[id] || now.Sub(at) >= pendingTTL {
			delete(next.Pending, id)
		}
	}
	next.XIDTimes = recentXIDs(s.XIDTimes, now)
	next.TenantsIssued = max(s.TenantsIssued, highestTenant(snap.Reservations))
	return next
}

func highestTenant(reservations []ReservationInfo) int {
	highest := 0
	for _, r := range reservations {
		rest, ok := strings.CutPrefix(r.Tenant, tenantPrefix)
		if n, err := strconv.Atoi(rest); ok && err == nil {
			highest = max(highest, n)
		}
	}
	return highest
}

// Attempted records that a was tried, whether or not it worked, so a failing
// target is not hammered every tick.
func (s State) Attempted(a Action, now time.Time) State {
	next := s.clone()
	next.Last[a.Kind] = now
	switch a.Kind {
	case KindXID:
		next.XIDTimes = append(recentXIDs(s.XIDTimes, now), now)
	case KindReserve:
		next.TenantsIssued++
	}
	return next
}

// Applied records the lasting effect of an action that succeeded.
func (s State) Applied(a Action, now time.Time) State {
	next := s.clone()
	switch a.Kind {
	case KindXID, KindImplant:
		next.Pending[a.Node] = now
	case KindRepair:
		delete(next.Pending, a.Node)
		delete(next.QuarantinedSince, a.Node)
	}
	return next
}

func (s State) clone() State {
	return State{
		Last:             cloneMap(s.Last),
		XIDTimes:         slices.Clone(s.XIDTimes),
		Pending:          cloneMap(s.Pending),
		QuarantinedSince: cloneMap(s.QuarantinedSince),
		TenantsIssued:    s.TenantsIssued,
	}
}

// cloneMap, unlike maps.Clone, returns a map that is safe to write to even
// when m is nil.
func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	maps.Copy(out, m)
	return out
}

func recentXIDs(times []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range times {
		if now.Sub(t) < xidWindow {
			out = append(out, t)
		}
	}
	return out
}

func (s State) xidsInWindow(now time.Time) int {
	return len(recentXIDs(s.XIDTimes, now))
}

func (s State) isPending(id string, now time.Time) bool {
	at, ok := s.Pending[id]
	return ok && now.Sub(at) < pendingTTL
}

// due reports whether interval has passed since last; a zero last is due.
func due(last, now time.Time, interval time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= interval
}
