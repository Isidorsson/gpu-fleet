// The control plane's public, read-only API answered from the replay's
// simulated fleet. Routes, JSON field order and error bodies follow
// controlplane/internal/api.

import { POLICIES, STATES } from "./sim.js";

const MAX_REMEDIATIONS = 500, DEFAULT_REMEDIATIONS = 50;
const SIGNING_KEY = "u8pM4l1xLrQ7cY2dW9sT5vK3hN6bF0jZaE1gR8oP2iU=";

const routes = [
  [/^\/healthz$/, () => text(200, "")],
  [/^\/metrics$/, fleet => text(200, metrics(fleet))],
  [/^\/v1\/nodes$/, (fleet, q) => json(200, { nodes: [...fleet.nodes.values()].filter(n => !q.get("state") || n.state === q.get("state")).map(nodeJSON) })],
  [/^\/v1\/nodes\/([^/]+)$/, (fleet, _, id) => withNode(fleet, id, n => json(200, nodeJSON(n)))],
  [/^\/v1\/nodes\/([^/]+)\/events$/, (fleet, _, id) => withNode(fleet, id, n => json(200, { events: n.events }))],
  [/^\/v1\/nodes\/([^/]+)\/attestation$/, (fleet, _, id) => {
    const result = fleet.attestations.get(id);
    return result ? json(200, result) : json(404, { error: "attestation not found" });
  }],
  [/^\/v1\/attestation\/keys$/, () => json(200, { keys: [{ alg: "Ed25519", public_key: SIGNING_KEY }] })],
  [/^\/v1\/reservations$/, (fleet, q) => json(200, { reservations: fleet.reservations.filter(r => !q.get("tenant") || r.tenant === q.get("tenant")).map(reservationJSON) })],
  [/^\/v1\/reservations\/([^/]+)$/, (fleet, _, id) => {
    const res = fleet.reservations.find(r => r.id === id);
    return res ? json(200, reservationJSON(res)) : json(404, { error: "reservation not found" });
  }],
  [/^\/v1\/capacity$/, fleet => json(200, capacity(fleet))],
  [/^\/v1\/remediations$/, (fleet, q) => {
    const limit = q.has("limit") ? Number(q.get("limit")) : DEFAULT_REMEDIATIONS;
    if (!Number.isInteger(limit) || limit < 1 || limit > MAX_REMEDIATIONS) {
      return json(400, { error: `bad request: limit must be an integer from 1 to ${MAX_REMEDIATIONS}` });
    }
    return json(200, { remediations: fleet.remediations.slice(0, limit) });
  }],
];

export function handle(fleet, method, url) {
  if (method !== "GET" && method !== "HEAD") return json(405, { error: "this is a read-only public endpoint" });
  const { pathname, searchParams } = new URL(url, "http://controlplane");
  for (const [pattern, route] of routes) {
    const match = pathname.match(pattern);
    if (match) return route(fleet, searchParams, ...match.slice(1));
  }
  return text(404, "404 page not found\n");
}

const json = (status, body) => ({ status, type: "json", body });
const text = (status, body) => ({ status, type: "text", body });

function withNode(fleet, id, ok) {
  const n = fleet.nodes.get(id);
  return n ? ok(n) : json(404, { error: "node not found" });
}

function nodeJSON(n) {
  return {
    id: n.id, pod: n.pod, rack: n.rack, bmc: "https://bmcsim:8443", ak_pub: akPub(n.id), state: n.state,
    ...(n.tenant && { tenant: n.tenant }),
  };
}

function reservationJSON(r) {
  return {
    id: r.id, tenant: r.tenant, count: r.count, policy: r.policy, starts_at: r.starts_at, ends_at: r.ends_at,
    status: r.status, nodes: r.nodes, created_at: r.created_at,
  };
}

export function capacity(fleet) {
  const pods = {};
  for (const n of fleet.nodes.values()) {
    const pod = (pods[n.pod] ??= { pod: n.pod, free: 0, racks: {} });
    pod.racks[n.rack] ??= 0;
    if (n.state === "available") { pod.free++; pod.racks[n.rack]++; }
  }
  const list = Object.values(pods).map(p => ({ pod: p.pod, free: p.free, largest_rack: Math.max(...Object.values(p.racks)) }));
  return { pods: list, total_free: list.reduce((sum, p) => sum + p.free, 0) };
}

function metrics(fleet) {
  const counts = Object.fromEntries(STATES.map(s => [s, 0]));
  for (const n of fleet.nodes.values()) counts[n.state]++;
  const statuses = { active: 0, degraded: 0, ended: 0 };
  for (const r of fleet.reservations) statuses[r.status]++;
  const { attestations, placementFailures, remediations, degraded } = fleet.counters;
  return [
    "# TYPE fleet_nodes gauge",
    ...STATES.map(s => `fleet_nodes{state="${s}"} ${counts[s]}`),
    "# TYPE capacity_free_nodes gauge",
    ...capacity(fleet).pods.map(p => `capacity_free_nodes{pod="${p.pod}"} ${p.free}`),
    "# TYPE reservations gauge",
    ...Object.entries(statuses).map(([s, v]) => `reservations{status="${s}"} ${v}`),
    "# TYPE attestations_total counter",
    `attestations_total{verdict="pass"} ${attestations.pass}`,
    `attestations_total{verdict="fail"} ${attestations.fail}`,
    "# TYPE placement_failures_total counter",
    ...POLICIES.map(p => `placement_failures_total{policy="${p}"} ${placementFailures[p]}`),
    "# TYPE remediation_actions_total counter",
    ...Object.entries(remediations).map(([o, v]) => `remediation_actions_total{outcome="${o}"} ${v}`),
    "# TYPE reservation_degraded_total counter",
    `reservation_degraded_total ${degraded}`,
    "",
  ].join("\n");
}

// akPub derives a stable, plausible base64 Ed25519 public key per node.
function akPub(id) {
  let h = 2166136261;
  const bytes = [];
  for (let i = 0; i < 32; i++) {
    h = Math.imul(h ^ id.charCodeAt(i % id.length) ^ i, 16777619) >>> 0;
    bytes.push(h & 0xff);
  }
  return btoa(String.fromCharCode(...bytes));
}
