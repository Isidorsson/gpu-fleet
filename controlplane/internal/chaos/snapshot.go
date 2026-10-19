package chaos

import (
	"context"
	"fmt"

	"gpu-fleet/controlplane/internal/node"
)

// ReservationStatus mirrors the API's reservation status strings. They are
// redeclared here because internal/reservation links the Postgres driver.
type ReservationStatus string

const (
	ReservationScheduled ReservationStatus = "scheduled"
	ReservationActive    ReservationStatus = "active"
	ReservationDegraded  ReservationStatus = "degraded"
)

// NodeInfo is a node as GET /v1/nodes reports it.
type NodeInfo struct {
	ID     string     `json:"id"`
	Pod    string     `json:"pod"`
	Rack   string     `json:"rack"`
	State  node.State `json:"state"`
	Tenant string     `json:"tenant"`
}

// ReservationInfo is a reservation as GET /v1/reservations reports it.
type ReservationInfo struct {
	ID     string            `json:"id"`
	Tenant string            `json:"tenant"`
	Status ReservationStatus `json:"status"`
	Nodes  []string          `json:"nodes"`
}

// open reports whether the reservation still holds or will hold capacity.
func (r ReservationInfo) open() bool {
	switch r.Status {
	case ReservationScheduled, ReservationActive, ReservationDegraded:
		return true
	}
	return false
}

// Snapshot is what the planner sees of the world on one tick. AgentBacked
// lists the nodes that have a GPU agent chaos can inject XIDs into.
type Snapshot struct {
	Nodes        []NodeInfo
	Reservations []ReservationInfo
	AgentBacked  map[string]bool
}

func takeSnapshot(ctx context.Context, cp *ControlPlane, agentBacked map[string]bool) (Snapshot, error) {
	nodes, err := cp.Nodes(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list nodes: %w", err)
	}
	reservations, err := cp.Reservations(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list reservations: %w", err)
	}
	return Snapshot{Nodes: nodes, Reservations: reservations, AgentBacked: agentBacked}, nil
}
