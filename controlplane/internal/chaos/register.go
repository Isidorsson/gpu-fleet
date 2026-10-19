package chaos

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Topology mirrors deploy/seed.sh: 8 nodes per rack, racks r1 to r4 in pod
// p1, the rest in p2. The rack follows from the number in the node ID.
const (
	nodesPerRack = 8
	pod1Racks    = 4
	pod1         = "p1"
	pod2         = "p2"
)

// Topology places a node by the number after the last dash of its ID.
func Topology(id string) (pod, rack string, err error) {
	suffix := id[strings.LastIndex(id, "-")+1:]
	n, convErr := strconv.Atoi(suffix)
	if convErr != nil || n < 1 {
		return "", "", fmt.Errorf("node ID %q does not end in a positive number", id)
	}
	rackNumber := (n-1)/nodesPerRack + 1
	pod = pod1
	if rackNumber > pod1Racks {
		pod = pod2
	}
	return pod, fmt.Sprintf("r%d", rackNumber), nil
}

// Startup waits for the control plane and bmcsim, then registers the fleet.
func (s *Service) Startup(ctx context.Context) error {
	if err := s.backoff.Retry(ctx, "wait for control plane", s.cp.Healthz); err != nil {
		return err
	}
	var ids []string
	err := s.backoff.Retry(ctx, "wait for bmcsim systems", func(ctx context.Context) error {
		var listErr error
		ids, listErr = s.bmc.Systems(ctx)
		return listErr
	})
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("bmcsim lists no systems")
	}
	return s.registerAll(ctx, ids)
}

func (s *Service) registerAll(ctx context.Context, ids []string) error {
	created := 0
	for _, id := range ids {
		isNew, err := s.registerOne(ctx, id)
		if err != nil {
			return err
		}
		if isNew {
			created++
		}
	}
	s.log.Info("fleet registered", "systems", len(ids), "created", created, "already_registered", len(ids)-created)
	return nil
}

func (s *Service) registerOne(ctx context.Context, id string) (bool, error) {
	pod, rack, err := Topology(id)
	if err != nil {
		return false, err
	}
	var ak string
	err = s.backoff.Retry(ctx, "read identity of "+id, func(ctx context.Context) error {
		var readErr error
		ak, readErr = s.bmc.AKPub(ctx, id)
		return readErr
	})
	if err != nil {
		return false, err
	}
	spec := NodeSpec{ID: id, Pod: pod, Rack: rack, BMC: s.cfg.NodeBMCURL, AKPub: ak}
	var created bool
	err = s.backoff.Retry(ctx, "register "+id, func(ctx context.Context) error {
		var regErr error
		created, regErr = s.cp.RegisterNode(ctx, spec)
		return regErr
	})
	return created, err
}
