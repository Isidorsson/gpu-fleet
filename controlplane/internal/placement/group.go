package placement

import (
	"cmp"
	"slices"
)

type rackGroup struct {
	name string
	ids  []string // ascending
}

func (r rackGroup) take(n int) []string { return slices.Clone(r.ids[:n]) }

type podGroup struct {
	name  string
	racks []rackGroup // ascending by name
	free  int
}

func (p podGroup) largestRack() int {
	largest := 0
	for _, r := range p.racks {
		largest = max(largest, len(r.ids))
	}
	return largest
}

// groupPods returns pods ascending by name, each holding its racks ascending
// by name and each rack holding its node IDs ascending. A rack is identified
// by (pod, rack): the same rack name in two pods is two racks.
func groupPods(free []Candidate) []podGroup {
	byPod := make(map[string]map[string][]string)
	for _, c := range free {
		racks, ok := byPod[c.Pod]
		if !ok {
			racks = make(map[string][]string)
			byPod[c.Pod] = racks
		}
		racks[c.Rack] = append(racks[c.Rack], c.NodeID)
	}
	pods := make([]podGroup, 0, len(byPod))
	for name, racks := range byPod {
		pods = append(pods, newPodGroup(name, racks))
	}
	slices.SortFunc(pods, func(a, b podGroup) int { return cmp.Compare(a.name, b.name) })
	return pods
}

func newPodGroup(name string, racks map[string][]string) podGroup {
	p := podGroup{name: name, racks: make([]rackGroup, 0, len(racks))}
	for rack, ids := range racks {
		slices.Sort(ids)
		p.racks = append(p.racks, rackGroup{name: rack, ids: ids})
		p.free += len(ids)
	}
	slices.SortFunc(p.racks, func(a, b rackGroup) int { return cmp.Compare(a.name, b.name) })
	return p
}

// Summarize returns one entry per pod present in free, ascending by pod.
func Summarize(free []Candidate) []PodCapacity {
	pods := groupPods(free)
	out := make([]PodCapacity, 0, len(pods))
	for _, p := range pods {
		out = append(out, PodCapacity{Pod: p.name, Free: p.free, LargestRack: p.largestRack()})
	}
	return out
}
