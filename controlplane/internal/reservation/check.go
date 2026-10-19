package reservation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpu-fleet/controlplane/internal/node"
)

// check keeps a running reservation whole. A node that left it is replaced
// for the rest of the window, or dropped when nothing fits, and the
// reservation stays degraded from then on. A replacement that is not ready
// yet is provisioned on a later tick.
func (r *Reconciler) check(ctx context.Context, res Reservation, now time.Time) error {
	var errs []error
	for _, id := range res.NodeIDs {
		n, err := r.nodes.Get(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("get node %s: %w", id, err))
			continue
		}
		if !lost(n, res) {
			_, err = r.provisionNode(ctx, res, n, now)
		} else if res.Status, err = r.degrade(ctx, res); err == nil {
			err = r.replaceLost(ctx, res, id, now)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// lost reports whether n has left res: quarantined, or being wiped after it
// was provisioned for res (provisioning failed, or it was released).
func lost(n node.Node, res Reservation) bool {
	return n.State == node.StateQuarantined || (n.State == node.StateWiping && n.Reservation == res.ID)
}

// degrade returns the reservation's status after marking it degraded. Only
// the change from active is counted, and only once it is stored.
func (r *Reconciler) degrade(ctx context.Context, res Reservation) (Status, error) {
	if res.Status != StatusActive {
		return res.Status, nil
	}
	if err := r.setStatus(ctx, res.ID, StatusDegraded); err != nil {
		return res.Status, err
	}
	r.metrics.Degraded()
	r.log.Warn("reservation degraded mid-run", "reservation", res.ID)
	return StatusDegraded, nil
}

func (r *Reconciler) replaceLost(ctx context.Context, res Reservation, id string, now time.Time) error {
	out, err := r.replaceNode(ctx, res, id, now)
	if err != nil || out != nodeUnreplaceable {
		return err
	}
	return r.dropNode(ctx, res.ID, id)
}
