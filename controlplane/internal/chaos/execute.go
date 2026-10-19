package chaos

import (
	"context"
	"fmt"
	"net/http"

	"gpu-fleet/controlplane/internal/node"
)

// Executor applies an Action by calling the services. Agents maps a node ID
// to its GPU agent.
type Executor struct {
	cp     *ControlPlane
	bmc    *BMC
	agents map[string]*Agent
}

func NewExecutor(cp *ControlPlane, bmc *BMC, agents map[string]*Agent) *Executor {
	return &Executor{cp: cp, bmc: bmc, agents: agents}
}

// Execute applies a and describes the outcome. A reservation refused for lack
// of capacity is an outcome, not an error.
func (e *Executor) Execute(ctx context.Context, a Action) (string, error) {
	switch a.Kind {
	case KindRepair:
		return e.repair(ctx, a)
	case KindXID:
		return e.injectXID(ctx, a)
	case KindImplant:
		return e.implant(ctx, a)
	case KindReserve:
		return e.reserve(ctx, a)
	}
	return "", fmt.Errorf("unknown action kind %q", a.Kind)
}

func (e *Executor) reserve(ctx context.Context, a Action) (string, error) {
	res, err := e.cp.Reserve(ctx, ReserveRequest{
		Tenant: a.Tenant, Count: a.Count, Policy: a.Policy,
		StartsAt: a.StartsAt.UTC(), EndsAt: a.EndsAt.UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("reserve for %s: %w", a.Tenant, err)
	}
	if !res.Created {
		return fmt.Sprintf("no capacity, largest block %d", res.Largest), nil
	}
	return "created " + res.ID, nil
}

func (e *Executor) implant(ctx context.Context, a Action) (string, error) {
	if err := e.bmc.Tamper(ctx, a.Node, a.Component); err != nil {
		return "", fmt.Errorf("tamper %s on %s: %w", a.Component, a.Node, err)
	}
	return "tampered, caught at the next attestation", nil
}

func (e *Executor) injectXID(ctx context.Context, a Action) (string, error) {
	agent, ok := e.agents[a.Node]
	if !ok {
		return "", fmt.Errorf("xid on %s: no agent configured", a.Node)
	}
	if err := agent.InjectXID(ctx, a.GPU, a.Code); err != nil {
		return "", fmt.Errorf("xid on %s: %w", a.Node, err)
	}
	return "injected, remediation quarantines it once the alert fires", nil
}

// repair undoes whatever put the node in quarantine, then tells the control
// plane. The event goes last, so a failure earlier leaves the node
// quarantined and the whole repair is retried on the next tick.
func (e *Executor) repair(ctx context.Context, a Action) (string, error) {
	if err := e.bmc.ClearTamper(ctx, a.Node); err != nil {
		return "", fmt.Errorf("clear tamper on %s: %w", a.Node, err)
	}
	if agent, ok := e.agents[a.Node]; ok {
		if err := agent.ClearXIDs(ctx); err != nil {
			return "", fmt.Errorf("clear xids on %s: %w", a.Node, err)
		}
	}
	err := e.cp.SendEvent(ctx, a.Node, node.EventRepaired)
	if hasStatus(err, http.StatusConflict) {
		return "no longer quarantined", nil
	}
	if err != nil {
		return "", fmt.Errorf("repaired event for %s: %w", a.Node, err)
	}
	return "repaired, wiping next", nil
}
