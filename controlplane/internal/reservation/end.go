package reservation

import (
	"context"
	"errors"
	"fmt"

	"gpu-fleet/controlplane/internal/node"
)

// end releases the tenant's active nodes and marks the reservation ended once
// none is provisioning or active any more. A provisioning node cannot be
// released, so it holds the reservation open until a later tick.
func (r *Reconciler) end(ctx context.Context, res Reservation) error {
	var held int
	var errs []error
	for _, id := range res.NodeIDs {
		stillHeld, err := r.releaseNode(ctx, res, id)
		if err != nil {
			errs = append(errs, err)
		}
		if err != nil || stillHeld {
			held++
		}
	}
	if held > 0 {
		return errors.Join(errs...)
	}
	return r.setStatus(ctx, res.ID, StatusEnded)
}

// releaseNode reports whether the node is still provisioning or active for
// the reservation's tenant after this step.
func (r *Reconciler) releaseNode(ctx context.Context, res Reservation, id string) (bool, error) {
	n, err := r.nodes.Get(ctx, id)
	if err != nil {
		return true, fmt.Errorf("get node %s: %w", id, err)
	}
	switch {
	case !heldBy(n, res.Tenant):
		return false, nil
	case n.State == node.StateProvisioning:
		return true, nil
	}
	_, err = r.nodes.ApplyEvent(ctx, id, node.EventRelease, "")
	if errors.Is(err, node.ErrInvalidTransition) {
		r.log.Warn("node changed before release, retrying next tick", "reservation", res.ID, "node", id, "error", err)
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("release node %s: %w", id, err)
	}
	return false, nil
}
