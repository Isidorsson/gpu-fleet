-- +goose Up
CREATE TABLE nodes (
    id               text PRIMARY KEY,
    rack             text NOT NULL,
    bmc              text NOT NULL,
    state            text NOT NULL CHECK (state IN (
                         'racked', 'inspecting', 'available', 'provisioning',
                         'active', 'wiping', 'quarantined')),
    tenant           text,
    lease_owner      text,
    lease_expires_at timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- Defense in depth for node.Apply: a node being provisioned or in use
    -- always has an owner.
    CHECK (state NOT IN ('provisioning', 'active') OR tenant IS NOT NULL),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX nodes_state_idx ON nodes (state);

-- Serves ClaimWork: the states that need a worker, oldest first.
CREATE INDEX nodes_claimable_idx ON nodes (updated_at)
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

-- +goose Down
DROP TABLE node_events;
DROP TABLE nodes;
