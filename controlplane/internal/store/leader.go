package store

import "gpu-fleet/controlplane/internal/reservation"

// NewLeader returns an advisory-lock leader on this store's pool, so callers
// never need the pool itself.
func (p *Postgres) NewLeader(key int64) *reservation.PostgresLeader {
	return reservation.NewPostgresLeader(p.pool, key)
}
