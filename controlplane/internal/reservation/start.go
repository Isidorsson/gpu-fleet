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
	// nodeStarted: provisioning or active for the reservation's tenant.
	nodeStarted nodeOutcome = iota
	// nodePending: not ready yet, or the step failed; retry next tick.
	nodePending
	// nodeDropped: quarantined and no replacement fits.
	nodeDropped
)

// start provisions every node of a reservation whose window has begun. The
// reservation only leaves scheduled once no node is pending, so a slow
// inspection or a failed call just delays it by a tick.
func (r *Reconciler) start(ctx context.Context, res Reservation, now time.Time) error {
	if !res.EndsAt.After(now) {
		r.log.Warn("reservation window passed before it started", "reservation", res.ID)
		return r.setStatus(ctx, res.ID, StatusEnded)
	}
	var pending, dropped int
	var errs []error
	for _, id := range res.NodeIDs {
		out, err := r.startNode(ctx, res, id, now)
		if err != nil {
			errs = append(errs, err)
		}
		switch {
		case err != nil || out == nodePending:
			pending++
		case out == nodeDropped:
			dropped++
		}
	}
	if pending > 0 {
		return errors.Join(errs...)
	}
	return r.markStarted(ctx, res.ID, dropped)
}

// markStarted reports Degraded only after the status is stored, so a failed
// write is retried without counting the reservation twice.
func (r *Reconciler) markStarted(ctx context.Context, id string, dropped int) error {
	if dropped == 0 {
		return r.setStatus(ctx, id, StatusActive)
	}
	if err := r.setStatus(ctx, id, StatusDegraded); err != nil {
		return err
	}
	r.metrics.Degraded()
	r.log.Warn("reservation degraded", "reservation", id, "dropped_nodes", dropped)
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
	return r.provisionNode(ctx, res, n)
}

func (r *Reconciler) replaceNode(ctx context.Context, res Reservation, oldID string, now time.Time) (nodeOutcome, error) {
	newID, err := r.store.ReplaceNode(ctx, res.ID, oldID, now, r.place)
	var capacity *placement.CapacityError
	if errors.As(err, &capacity) {
		r.log.Warn("no replacement for quarantined node", "reservation", res.ID, "node", oldID, "largest", capacity.Largest)
		return nodeDropped, nil
	}
	if err != nil {
		return nodePending, fmt.Errorf("replace node %s: %w", oldID, err)
	}
	r.log.Info("replaced quarantined node", "reservation", res.ID, "old", oldID, "new", newID)
	n, err := r.nodes.Get(ctx, newID)
	if err != nil {
		return nodePending, fmt.Errorf("get replacement node %s: %w", newID, err)
	}
	return r.provisionNode(ctx, res, n)
}

func (r *Reconciler) provisionNode(ctx context.Context, res Reservation, n node.Node) (nodeOutcome, error) {
	switch {
	case n.State == node.StateAvailable:
		return r.applyProvision(ctx, res, n.ID)
	case heldBy(n, res.Tenant):
		return nodeStarted, nil
	default:
		r.log.Debug("node not ready", "reservation", res.ID, "node", n.ID, "state", n.State, "tenant", n.Tenant)
		return nodePending, nil
	}
}

func (r *Reconciler) applyProvision(ctx context.Context, res Reservation, id string) (nodeOutcome, error) {
	_, err := r.nodes.ApplyEvent(ctx, id, node.EventProvision, res.Tenant)
	if errors.Is(err, node.ErrInvalidTransition) {
		r.log.Warn("node changed before provision, retrying next tick", "reservation", res.ID, "node", id, "error", err)
		return nodePending, nil
	}
	if err != nil {
		return nodePending, fmt.Errorf("provision node %s: %w", id, err)
	}
	return nodeStarted, nil
}

// heldBy reports whether the tenant is provisioning or running on n.
func heldBy(n node.Node, tenant string) bool {
	return n.Tenant == tenant && (n.State == node.StateProvisioning || n.State == node.StateActive)
}
