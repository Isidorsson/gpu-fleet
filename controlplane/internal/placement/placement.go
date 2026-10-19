package placement

import (
	"cmp"
	"fmt"
	"slices"
)

// Place returns the IDs of req.Count nodes from free, ascending, chosen by the
// rules in docs/reservations.md. It does not modify free.
func Place(req Request, free []Candidate) ([]string, error) {
	if err := validate(req, free); err != nil {
		return nil, err
	}
	pods := groupPods(free)
	ids, ok := choose(req, pods)
	if !ok {
		return nil, &CapacityError{
			Policy:    req.Policy,
			Requested: req.Count,
			Largest:   largestBlock(req.Policy, pods),
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func validate(req Request, free []Candidate) error {
	if req.Count < 1 {
		return fmt.Errorf("%w: count %d is less than 1", ErrInvalidRequest, req.Count)
	}
	if _, err := ParsePolicy(string(req.Policy)); err != nil {
		return err
	}
	return validateCandidates(free)
}

func validateCandidates(free []Candidate) error {
	seen := make(map[string]struct{}, len(free))
	for _, c := range free {
		if c.NodeID == "" || c.Pod == "" || c.Rack == "" {
			return fmt.Errorf("%w: candidate %+v has an empty field", ErrInvalidRequest, c)
		}
		if _, dup := seen[c.NodeID]; dup {
			return fmt.Errorf("%w: duplicate node %q", ErrInvalidRequest, c.NodeID)
		}
		seen[c.NodeID] = struct{}{}
	}
	return nil
}

func choose(req Request, pods []podGroup) ([]string, bool) {
	switch req.Policy {
	case PolicySameRack:
		return placeSameRack(req.Count, pods)
	case PolicySamePod:
		return placeSamePod(req.Count, pods)
	case PolicyAny:
		return placeAny(req.Count, pods)
	}
	return nil, false
}

func largestBlock(policy Policy, pods []podGroup) int {
	largest, total := 0, 0
	for _, p := range pods {
		total += p.free
		switch policy {
		case PolicySameRack:
			largest = max(largest, p.largestRack())
		case PolicySamePod:
			largest = max(largest, p.free)
		}
	}
	if policy == PolicyAny {
		return total
	}
	return largest
}

func placeSameRack(count int, pods []podGroup) ([]string, bool) {
	var best *rackGroup
	for i := range pods {
		r := bestFitRack(pods[i].racks, count)
		if r != nil && (best == nil || len(r.ids) < len(best.ids)) {
			best = r
		}
	}
	if best == nil {
		return nil, false
	}
	return best.take(count), true
}

func placeSamePod(count int, pods []podGroup) ([]string, bool) {
	var best *podGroup
	for i := range pods {
		p := &pods[i]
		if p.free >= count && (best == nil || p.free < best.free) {
			best = p
		}
	}
	if best == nil {
		return nil, false
	}
	return fillPod(*best, count), true
}

func placeAny(count int, pods []podGroup) ([]string, bool) {
	if ids, ok := placeSamePod(count, pods); ok {
		return ids, true
	}
	return spanPods(count, pods)
}

// bestFitRack returns the rack with the fewest free nodes that still has at
// least count, the first (lowest name) on ties, or nil. racks must be ascending
// by name.
func bestFitRack(racks []rackGroup, count int) *rackGroup {
	var best *rackGroup
	for i := range racks {
		r := &racks[i]
		if len(r.ids) >= count && (best == nil || len(r.ids) < len(best.ids)) {
			best = r
		}
	}
	return best
}

// fillPod takes count nodes from p, which must have at least count free: one
// best-fit rack when any rack fits, else racks in ascending free count, each
// consumed fully before the next.
func fillPod(p podGroup, count int) []string {
	if r := bestFitRack(p.racks, count); r != nil {
		return r.take(count)
	}
	racks := slices.Clone(p.racks)
	slices.SortFunc(racks, func(a, b rackGroup) int {
		return cmp.Or(cmp.Compare(len(a.ids), len(b.ids)), cmp.Compare(a.name, b.name))
	})
	ids := make([]string, 0, count)
	for _, r := range racks {
		ids = append(ids, r.take(min(count-len(ids), len(r.ids)))...)
		if len(ids) == count {
			break
		}
	}
	return ids
}

// spanPods consumes pods in descending free count, so the fewest pods are used.
func spanPods(count int, pods []podGroup) ([]string, bool) {
	order := slices.Clone(pods)
	slices.SortFunc(order, func(a, b podGroup) int {
		return cmp.Or(cmp.Compare(b.free, a.free), cmp.Compare(a.name, b.name))
	})
	ids := make([]string, 0, count)
	for _, p := range order {
		ids = append(ids, fillPod(p, min(count-len(ids), p.free))...)
		if len(ids) == count {
			return ids, true
		}
	}
	return nil, false
}
