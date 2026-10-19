-- +goose Up
-- The extension is database-wide, so it is pinned to public: installed in the
-- first schema of the search_path it would be dropped with a per-test schema.
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

CREATE TABLE nodes (
    id               text PRIMARY KEY,
    pod              text NOT NULL,
    rack             text NOT NULL,
    bmc              text NOT NULL,
    state            text NOT NULL CHECK (state IN (
                         'racked', 'inspecting', 'available', 'provisioning',
                         'active', 'wiping', 'quarantined')),
    tenant           text,
    -- The reservation the node was provisioned for. Kept, like tenant, until
    -- a successful wipe; the foreign key is added with reservations below.
    reservation_id   text,
    lease_owner      text,
    lease_expires_at timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- Claims since the last applied transition. A node whose workers keep
    -- dying mid-task sorts behind nodes that are making progress.
    claim_attempts   integer NOT NULL DEFAULT 0 CHECK (claim_attempts >= 0),
    -- Defense in depth for node.Apply: a node being provisioned or in use
    -- always has an owner.
    CHECK (state NOT IN ('provisioning', 'active') OR tenant IS NOT NULL),
    CHECK (reservation_id IS NULL OR tenant IS NOT NULL),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX nodes_state_idx ON nodes (state);

-- Serves ClaimWork: the states that need a worker, least-retried then
-- oldest first.
CREATE INDEX nodes_claimable_idx ON nodes (claim_attempts, updated_at, id)
    WHERE state IN ('racked', 'inspecting', 'provisioning', 'wiping');

CREATE TABLE node_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    node_id    text NOT NULL REFERENCES nodes (id),
    from_state text NOT NULL,
    event      text NOT NULL,
    to_state   text NOT NULL,
    tenant     text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX node_events_node_id_idx ON node_events (node_id, id);

CREATE TABLE reservations (
    id         text PRIMARY KEY,
    tenant     text NOT NULL,
    node_count integer NOT NULL CHECK (node_count BETWEEN 1 AND 64),
    policy     text NOT NULL CHECK (policy IN ('same_rack', 'same_pod', 'any')),
    period     tstzrange NOT NULL CHECK (NOT isempty(period)),
    status     text NOT NULL CHECK (status IN (
                   'scheduled', 'active', 'degraded', 'ended', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Serve the reconciler's DueToStart and DueToEnd scans.
CREATE INDEX reservations_due_start_idx ON reservations (lower(period))
    WHERE status = 'scheduled';
CREATE INDEX reservations_due_end_idx ON reservations (upper(period))
    WHERE status IN ('active', 'degraded');

CREATE INDEX reservations_tenant_idx ON reservations (tenant, created_at DESC);

ALTER TABLE nodes ADD FOREIGN KEY (reservation_id) REFERENCES reservations (id);
CREATE INDEX nodes_reservation_idx ON nodes (reservation_id) WHERE reservation_id IS NOT NULL;

-- A row's period is the span the reservation holds that node. It is shorter
-- than the reservation's own when the node replaced a failed one. The
-- exclusion constraint is what makes double-booking impossible: placement
-- races are settled here, not in application code.
CREATE TABLE reservation_nodes (
    reservation_id text NOT NULL REFERENCES reservations (id) ON DELETE CASCADE,
    node_id        text NOT NULL REFERENCES nodes (id),
    period         tstzrange NOT NULL CHECK (NOT isempty(period)),
    PRIMARY KEY (reservation_id, node_id),
    EXCLUDE USING gist (node_id WITH =, period WITH &&)
);

-- +goose Down
DROP TABLE reservation_nodes;
DROP TABLE reservations;
DROP TABLE node_events;
DROP TABLE nodes;
