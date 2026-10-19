-- +goose Up
CREATE TABLE nodes (
    id         text PRIMARY KEY,
    rack       text NOT NULL,
    state      text NOT NULL CHECK (state IN (
                   'racked', 'inspecting', 'available', 'provisioning',
                   'active', 'wiping', 'quarantined')),
    tenant     text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Defense in depth for node.Apply: a node being provisioned or in use
    -- always has an owner.
    CHECK (state NOT IN ('provisioning', 'active') OR tenant IS NOT NULL)
);

CREATE INDEX nodes_state_idx ON nodes (state);

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
