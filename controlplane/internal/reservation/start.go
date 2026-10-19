package reservation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpu-fleet/controlplane/internal/node"
	"gpu-fleet/controlplane/internal/placement"
)

type nodeOutcome int

const (
	// nodeStarted: provisioning or active for the reservation.
	nodeStarted nodeOutcome = iota
	// nodePending: not ready yet, or the step failed; retry next tick.
	nodePending
	// nodeUnreplaceable: it left the reservation and no replacement fits.
	nodeUnreplaceable
)

// start provisions every node of a reservation whose window has begun. The
// reservation only leaves scheduled once no node is pending, so a slow
// inspection or a failed call just delays it by a tick. A window that ran out
// first, for example because it was cancelled mid-start, is wound down.
func (r *Reconciler) start(ctx context.Context, res Reservation, now time.Time) error {
	if !res.EndsAt.After(now) {
		r.log.Warn("reservation window passed before it started", "reservation", res.ID)
		return r.end(ctx, res)
	}
	var pending int
	var unreplaceable []string
	var errs []error
	for _, id := range res.NodeIDs {
		out, err := r.startNode(ctx, res, id, now)
		if err != nil {
			errs = append(errs, err)
		}
		switch {
		case err != nil || out == nodePending:
			pending++
		case out == nodeUnreplaceable:
			unreplaceable = append(unreplaceable, id)
		}
	}
	if pending > 0 {
		return errors.Join(errs...)
	}
	for _, id := range unreplaceable {
		if err := r.dropNode(ctx, res.ID, id); err != nil {
			return err
		}
	}
	return r.markStarted(ctx, res, len(res.NodeIDs)-len(unreplaceable))
}

// markStarted reports Degraded only after the status is stored, so a failed
// write is retried without counting the reservation twice. Nodes dropped by
// an earlier attempt are already missing from kept, so a retry still degrades.
func (r *Reconciler) markStarted(ctx context.Context, res Reservation, kept int) error {
	if kept >= res.Count {
		return r.setStatus(ctx, res.ID, StatusActive)
	}
	if err := r.setStatus(ctx, res.ID, StatusDegraded); err != nil {
		return err
	}
	r.metrics.Degraded()
	r.log.Warn("reservation degraded", "reservation", res.ID, "nodes", kept, "count", res.Count)
	return nil
}

func (r *Reconciler) startNode(ctx context.Context, res Reservation, id string, now time.Time) (nodeOutcome, error) {
	n, err := r.nodes.Get(ctx, id)
	if err != nil {
		return nodePending, fmt.Errorf("get node %s: %w", id, err)
	}
	if n.State == node.StateQuarantined {
		return r.replaceNode(ctx, res, n.ID, now)
	}
	return r.provisionNode(ctx, res, n, now)
}

func (r *Reconciler) replaceNode(ctx context.Context, res Reservation, oldID string, now time.Time) (nodeOutcome, error) {
	newID, err := r.store.ReplaceNode(ctx, res.ID, oldID, now, r.place)
	var capacity *placement.CapacityError
	if errors.As(err, &capacity) {
		r.log.Warn("no replacement for node", "reservation", res.ID, "node", oldID, "largest", capacity.Largest)
		return nodeUnreplaceable, nil
	}
	if err != nil {
		return nodePending, fmt.Errorf("replace node %s: %w", oldID, err)
	}
	r.log.Info("replaced node", "reservation", res.ID, "old", oldID, "new", newID)
	n, err := r.nodes.Get(ctx, newID)
	if err != nil {
		return nodePending, fmt.Errorf("get replacement node %s: %w", newID, err)
	}
	return r.provisionNode(ctx, res, n, now)
}

func (r *Reconciler) dropNode(ctx context.Context, id, nodeID string) error {
	if err := r.store.DropNode(ctx, id, nodeID); err != nil {
		return fmt.Errorf("drop node %s: %w", nodeID, err)
	}
	r.log.Warn("dropped node from reservation", "reservation", id, "node", nodeID)
	return nil
}

func (r *Reconciler) provisionNode(ctx context.Context, res Reservation, n node.Node, now time.Time) (nodeOutcome, error) {
	switch {
	case n.State == node.StateAvailable:
		return r.applyStart(ctx, res, n.ID, now)
	case heldBy(n, res):
		return nodeStarted, nil
	default:
		r.log.Debug("node not ready", "reservation", res.ID, "node", n.ID, "state", n.State, "holder", n.Reservation)
		return nodePending, nil
	}
}

func (r *Reconciler) applyStart(ctx context.Context, res Reservation, id string, now time.Time) (nodeOutcome, error) {
	_, err := r.store.StartNode(ctx, res.ID, id, now)
	if errors.Is(err, node.ErrInvalidTransition) {
		r.log.Warn("node changed before provision, retrying next tick", "reservation", res.ID, "node", id, "error", err)
		return nodePending, nil
	}
	if err != nil {
		return nodePending, fmt.Errorf("start node %s: %w", id, err)
	}
	return nodeStarted, nil
}

// heldBy reports whether n is provisioning or running for res. Tenant
// equality is not enough: one tenant's back-to-back reservations share nodes.
func heldBy(n node.Node, res Reservation) bool {
	return n.Reservation == res.ID && (n.State == node.StateProvisioning || n.State == node.StateActive)
}
