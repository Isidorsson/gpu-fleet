// Package store persists nodes and their lifecycle history in Postgres.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"gpu-fleet/controlplane/internal/node"
)

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgExclusionViolation  = "23P01"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// EventRecord is one row of a node's audit log. Tenant is the tenant whose
// data the event concerned, so a wipe still records who was wiped.
type EventRecord struct {
	NodeID string
	From   node.State
	Event  node.Event
	To     node.State
	Tenant string
	At     time.Time
}

type Postgres struct {
	pool *pgxpool.Pool
	// now is the application clock (not the database's), used by the
	// reservation methods that are not passed the current time.
	now func() time.Time
}

func Open(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse postgres url: %w", err)
	}
	return openPool(ctx, cfg)
}

func openPool(ctx context.Context, cfg *pgxpool.Config) (*Postgres, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{pool: pool, now: time.Now}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Migrate(ctx context.Context) error {
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	db := stdlib.OpenDBFromPool(p.pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		return fmt.Errorf("migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

func (p *Postgres) Register(ctx context.Context, n node.Node) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO nodes (id, pod, rack, bmc, ak_pub, state, tenant) VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''))`,
		n.ID, n.Pod, n.Rack, n.BMC, n.AKPub, string(n.State), n.Tenant)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w: %s", ErrConflict, n.ID)
	}
	if err != nil {
		return fmt.Errorf("insert node %s: %w", n.ID, err)
	}
	return nil
}

const (
	nodeColumns = `id, pod, rack, bmc, ak_pub, state, coalesce(tenant, ''), coalesce(reservation_id, '')`
	selectNode  = `SELECT ` + nodeColumns + ` FROM nodes`
)

func (p *Postgres) Get(ctx context.Context, id string) (node.Node, error) {
	return getOne(ctx, p.pool, selectNode+` WHERE id = $1`, id, scanNode)
}

// List returns all nodes, or only those in state when it is non-empty.
func (p *Postgres) List(ctx context.Context, state node.State) ([]node.Node, error) {
	rows, err := p.pool.Query(ctx,
		selectNode+` WHERE ($1 = '' OR state = $1) ORDER BY id`, string(state))
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return pgx.CollectRows(rows, scanNode)
}

// CountByState returns how many nodes are in each state. States without nodes
// are absent from the map.
func (p *Postgres) CountByState(ctx context.Context) (map[node.State]int, error) {
	rows, err := p.pool.Query(ctx, `SELECT state, count(*) FROM nodes GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("count nodes by state: %w", err)
	}
	counts := map[node.State]int{}
	var state string
	var count int
	_, err = pgx.ForEachRow(rows, []any{&state, &count}, func() error {
		counts[node.State(state)] = count
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan state counts: %w", err)
	}
	return counts, nil
}

func (p *Postgres) Events(ctx context.Context, id string) ([]EventRecord, error) {
	if _, err := p.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx,
		`SELECT node_id, from_state, event, to_state, coalesce(tenant, ''), created_at
		 FROM node_events WHERE node_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("list events for %s: %w", id, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (EventRecord, error) {
		var r EventRecord
		var from, ev, to string
		err := row.Scan(&r.NodeID, &from, &ev, &to, &r.Tenant, &r.At)
		r.From, r.Event, r.To = node.State(from), node.Event(ev), node.State(to)
		return r, err
	})
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getOne[T any](ctx context.Context, q querier, sql, id string, scan pgx.RowToFunc[T]) (T, error) {
	var zero T
	rows, err := q.Query(ctx, sql, id)
	if err != nil {
		return zero, fmt.Errorf("get node %s: %w", id, err)
	}
	v, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return zero, fmt.Errorf("scan node %s: %w", id, err)
	}
	return v, nil
}

// nodeDests lists scan targets for nodeColumns, in order.
func nodeDests(n *node.Node, state *string) []any {
	return []any{&n.ID, &n.Pod, &n.Rack, &n.BMC, &n.AKPub, state, &n.Tenant, &n.Reservation}
}

func scanNode(row pgx.CollectableRow) (node.Node, error) {
	var n node.Node
	var state string
	err := row.Scan(nodeDests(&n, &state)...)
	n.State = node.State(state)
	return n, err
}
