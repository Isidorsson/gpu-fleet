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

var (
	ErrNotFound = errors.New("node not found")
	ErrConflict = errors.New("node already exists")
)

const pgUniqueViolation = "23505"

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
}

func Open(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
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
		`INSERT INTO nodes (id, rack, state, tenant) VALUES ($1, $2, $3, nullif($4, ''))`,
		n.ID, n.Rack, string(n.State), n.Tenant)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w: %s", ErrConflict, n.ID)
	}
	if err != nil {
		return fmt.Errorf("insert node %s: %w", n.ID, err)
	}
	return nil
}

const selectNode = `SELECT id, rack, state, coalesce(tenant, '') FROM nodes`

func (p *Postgres) Get(ctx context.Context, id string) (node.Node, error) {
	return getNode(ctx, p.pool, selectNode+` WHERE id = $1`, id)
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

// ApplyEvent locks the node row for the whole transition so concurrent
// events on one node are serialized and the audit log matches the state.
func (p *Postgres) ApplyEvent(ctx context.Context, id string, ev node.Event, tenant string) (node.Node, error) {
	var next node.Node
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		cur, err := getNode(ctx, tx, selectNode+` WHERE id = $1 FOR UPDATE`, id)
		if err != nil {
			return err
		}
		if next, err = cur.Apply(ev, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE nodes SET state = $2, tenant = nullif($3, ''), updated_at = now() WHERE id = $1`,
			id, string(next.State), next.Tenant); err != nil {
			return fmt.Errorf("update node %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO node_events (node_id, from_state, event, to_state, tenant)
			 VALUES ($1, $2, $3, $4, nullif($5, ''))`,
			id, string(cur.State), string(ev), string(next.State), affectedTenant(cur, next)); err != nil {
			return fmt.Errorf("insert event for %s: %w", id, err)
		}
		return nil
	})
	return next, err
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

func affectedTenant(before, after node.Node) string {
	if after.Tenant != "" {
		return after.Tenant
	}
	return before.Tenant
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getNode(ctx context.Context, q querier, sql string, id string) (node.Node, error) {
	rows, err := q.Query(ctx, sql, id)
	if err != nil {
		return node.Node{}, fmt.Errorf("get node %s: %w", id, err)
	}
	n, err := pgx.CollectExactlyOneRow(rows, scanNode)
	if errors.Is(err, pgx.ErrNoRows) {
		return node.Node{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return n, err
}

func scanNode(row pgx.CollectableRow) (node.Node, error) {
	var n node.Node
	var state string
	err := row.Scan(&n.ID, &n.Rack, &state, &n.Tenant)
	n.State = node.State(state)
	return n, err
}
