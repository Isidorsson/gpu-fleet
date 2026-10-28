// Browser replay of the demo: a seeded, in-memory model of the control plane
// and the chaos service. Rules and bounds follow controlplane/internal/chaos,
// node/lifecycle.go and docs/remediation.md; the data is simulated.

export const TICK_SECONDS = 10;
export const START = Date.UTC(2026, 9, 19, 12, 0, 0);
export const REFERENCE_VERSION = "2026-10-19.1";
export const STATES = ["racked", "inspecting", "available", "provisioning", "active", "wiping", "quarantined"];
export const POLICIES = ["same_rack", "same_pod", "any"];

const NODES = 50, PER_RACK = 8, P1_RACKS = 4;
const nodeId = n => "node-" + String(n).padStart(3, "0");

// The compose demo backs only a few nodes with a GPU agent. The replay gives
// the first node of every rack one, so faults land across the whole floor.
export const AGENT_NODES = new Set(Array.from({ length: Math.ceil(NODES / PER_RACK) }, (_, r) => nodeId(r * PER_RACK + 1)));

const SECOND = 1000, MINUTE = 60 * SECOND;
const DURATION = { provisioning: 20 * SECOND, wiping: 20 * SECOND, inspecting: 30 * SECOND };
const ALERT_DELAY = 88 * SECOND;          // XID to webhook: scrape, eval and GPUFatalXid's "for: 1m"
const RATE_LIMIT = { max: 5, window: 10 * MINUTE };
const CHAOS = {
  maxFaulted: 3, maxXidPerWindow: 3, xidWindow: 10 * MINUTE, xidInterval: 4 * MINUTE,
  implantInterval: 5 * MINUTE, repairAfter: 2 * MINUTE, pendingTTL: 15 * MINUTE,
  minOpen: 2, minCount: 4, maxCount: 12, minMinutes: 3, maxMinutes: 6,
};
const XID_CODES = [79, 48, 94];
const IMPLANTS = ["firmware", "bmc", "bootloader", "kernel", "gpu_vbios", "cc_mode"];
const HISTORY = (30 * MINUTE) / (TICK_SECONDS * SECOND);
const KEEP = { events: 20, log: 200, remediations: 500, quarantines: 200 };

export function createFleet(seed) {
  const fleet = {
    seed, now: START, rng: mulberry32(seed), ids: mulberry32(seed ^ 0x9e3779b9),
    nodes: new Map(), reservations: [], remediations: [], attestations: new Map(),
    alerts: [], log: [], history: [], quarantines: [],
    chaos: { last: {}, xidTimes: [], pending: new Map(), tenants: 0 },
    counters: {
      attestations: { pass: 0, fail: 0 }, placementFailures: { same_rack: 0, same_pod: 0, any: 0 },
      remediations: {}, degraded: 0,
    },
  };
  for (let n = 1; n <= NODES; n++) {
    const rack = Math.floor((n - 1) / PER_RACK) + 1;
    const id = nodeId(n);
    fleet.nodes.set(id, {
      id, pod: rack <= P1_RACKS ? "p1" : "p2", rack: "r" + rack, state: "available", since: START,
      tenant: null, reservation: null, tamper: null, xid: null,
      events: [{ from: "inspecting", event: "inspection_passed", to: "available", at: iso(START) }],
    });
    fleet.attestations.set(id, attestation(fleet, id, []));
  }
  sample(fleet);
  return fleet;
}

// tick is one chaos decision followed by ten seconds of platform time.
export function tick(fleet) {
  runChaos(fleet);
  advance(fleet);
}

export function advance(fleet) {
  fleet.now += TICK_SECONDS * SECOND;
  for (const res of fleet.reservations) {
    if (isOpen(res) && fleet.now >= res.endsAt) endReservation(fleet, res);
  }
  for (const n of fleet.nodes.values()) progress(fleet, n);
  const due = fleet.alerts.filter(a => a.firesAt <= fleet.now);
  fleet.alerts = fleet.alerts.filter(a => a.firesAt > fleet.now);
  for (const alert of due) remediate(fleet, alert);
  sample(fleet);
}

export function reserve(fleet, { tenant, count, policy, minutes }) {
  const placed = place(fleet, policy, count);
  if (!placed.ok) {
    fleet.counters.placementFailures[policy]++;
    return placed;
  }
  const res = {
    id: uuid(fleet), tenant, count, policy, starts_at: iso(fleet.now), ends_at: iso(fleet.now + minutes * MINUTE),
    endsAt: fleet.now + minutes * MINUTE, status: "active", nodes: [], created_at: iso(fleet.now),
  };
  fleet.reservations.push(res);
  for (const n of placed.nodes) provision(fleet, n, res);
  return { ok: true, reservation: res };
}

export function injectXid(fleet, id, gpu, code) {
  const n = fleet.nodes.get(id);
  if (!n || n.state !== "active" || !AGENT_NODES.has(id) || n.xid) return false;
  n.xid = { gpu, code, at: fleet.now };
  fleet.alerts.push({ node: id, firesAt: fleet.now + ALERT_DELAY, fingerprint: hex(fleet, 16) });
  return true;
}

export function plantImplant(fleet, id, component) {
  const n = fleet.nodes.get(id);
  if (!n || !IMPLANTS.includes(component)) return false;
  n.tamper = { component, gpu: Math.floor(fleet.ids() * 8) };
  return true;
}

export function repairNode(fleet, id) {
  const n = fleet.nodes.get(id);
  if (!n || n.state !== "quarantined") return false;
  n.tamper = null;
  n.xid = null;
  fleet.chaos.pending.delete(id);
  move(fleet, n, "repaired", "wiping");
  return true;
}

export const isOpen = res => res.status === "active" || res.status === "degraded";

// --- lifecycle --------------------------------------------------------------

function progress(fleet, n) {
  const elapsed = fleet.now - n.since;
  if (!(n.state in DURATION) || elapsed < DURATION[n.state]) return;
  if (n.state === "provisioning") return move(fleet, n, "provision_succeeded", "active");
  if (n.state === "wiping") return move(fleet, n, "wipe_succeeded", "inspecting");
  inspect(fleet, n);
}

function inspect(fleet, n) {
  if (n.xid) return quarantine(fleet, n, "inspection_failed");
  const reasons = n.tamper ? [reasonFor(n.tamper)] : [];
  fleet.attestations.set(n.id, attestation(fleet, n.id, reasons));
  fleet.counters.attestations[reasons.length ? "fail" : "pass"]++;
  if (reasons.length) return quarantine(fleet, n, "attestation_failed");
  move(fleet, n, "inspection_passed", "available");
}

function reasonFor({ component, gpu }) {
  if (component === "gpu_vbios") return `gpu ${gpu}: vbios 96.00.9F.00.07 not allowed`;
  if (component === "cc_mode") return `gpu ${gpu}: confidential computing mode off`;
  return `${component} digest not in reference values`;
}

function provision(fleet, n, res) {
  n.tenant = res.tenant;
  n.reservation = res.id;
  res.nodes.push(n.id);
  move(fleet, n, "provision", "provisioning");
}

function endReservation(fleet, res) {
  res.status = "ended";
  for (const id of res.nodes) {
    const n = fleet.nodes.get(id);
    if (n.reservation !== res.id) continue;
    move(fleet, n, "release", "wiping");
    n.tenant = null;
    n.reservation = null;
  }
}

function quarantine(fleet, n, cause) {
  const res = fleet.reservations.find(r => r.id === n.reservation && isOpen(r));
  move(fleet, n, cause, "quarantined");
  n.tenant = null;
  n.reservation = null;
  fleet.chaos.pending.delete(n.id);
  fleet.quarantines.unshift({ at: fleet.now, node: n.id, cause });
  fleet.quarantines.length = Math.min(fleet.quarantines.length, KEEP.quarantines);
  if (!res) return;
  res.status = "degraded";
  fleet.counters.degraded++;
  res.nodes = res.nodes.filter(id => id !== n.id);
  const [replacement] = bestFit(fleet, [...fleet.nodes.values()].filter(c => c.pod === n.pod && c.state === "available"), 1);
  if (replacement) provision(fleet, replacement, res);
}

function move(fleet, n, event, to) {
  n.events.push({ from: n.state, event, to, ...(n.tenant && { tenant: n.tenant }), at: iso(fleet.now) });
  if (n.events.length > KEEP.events) n.events.shift();
  n.state = to;
  n.since = fleet.now;
}

// --- remediation ------------------------------------------------------------

function remediate(fleet, alert) {
  const n = fleet.nodes.get(alert.node);
  const recent = fleet.remediations.filter(r => r.outcome === "quarantined" && alert.firesAt - Date.parse(r.at) < RATE_LIMIT.window);
  let outcome = "quarantined";
  if (n.state === "quarantined") outcome = "already_quarantined";
  else if (recent.length >= RATE_LIMIT.max) outcome = "rate_limited";
  fleet.remediations.unshift({
    node_id: n.id, alertname: "GPUFatalXid", fingerprint: alert.fingerprint, action: "quarantine", outcome, at: iso(alert.firesAt),
  });
  fleet.remediations.length = Math.min(fleet.remediations.length, KEEP.remediations);
  fleet.counters.remediations[outcome] = (fleet.counters.remediations[outcome] ?? 0) + 1;
  if (outcome === "quarantined") quarantine(fleet, n, "hardware_fault");
}

// --- placement --------------------------------------------------------------

function place(fleet, policy, count) {
  const free = [...fleet.nodes.values()].filter(n => n.state === "available");
  const groups = policy === "any" ? [free] : Object.values(groupBy(free, policy === "same_rack" ? "rack" : "pod"));
  const fits = groups.filter(g => g.length >= count).sort((a, b) => a.length - b.length);
  if (!fits.length) return { ok: false, largest: Math.max(0, ...groups.map(g => g.length)) };
  return { ok: true, nodes: bestFit(fleet, fits[0], count) };
}

// bestFit takes nodes from the racks with the fewest free nodes first, so
// whole racks stay free for rack-scale requests.
function bestFit(fleet, candidates, count) {
  const racks = Object.values(groupBy(candidates, "rack"))
    .sort((a, b) => a.length - b.length || a[0].rack.localeCompare(b[0].rack, undefined, { numeric: true }));
  return racks.flat().slice(0, count);
}

function groupBy(nodes, key) {
  const out = {};
  for (const n of nodes) (out[n[key]] ??= []).push(n);
  return out;
}

// --- chaos ------------------------------------------------------------------

function runChaos(fleet) {
  const action = planRepair(fleet) ?? planXid(fleet) ?? planImplant(fleet) ?? planReserve(fleet);
  if (!action) return;
  fleet.chaos.last[action.kind] = fleet.now;
  fleet.log.unshift({ at: iso(fleet.now), kind: action.kind, action: action.text, outcome: action.run() });
  fleet.log.length = Math.min(fleet.log.length, KEEP.log);
}

function planRepair(fleet) {
  const due = [...fleet.nodes.values()]
    .filter(n => n.state === "quarantined" && fleet.now - n.since >= CHAOS.repairAfter)
    .sort((a, b) => a.since - b.since || a.id.localeCompare(b.id));
  if (!due.length) return null;
  const id = due[0].id;
  return { kind: "repair", text: `repair ${id}`, run: () => (repairNode(fleet, id), "ok") };
}

function planXid(fleet) {
  const { chaos, now } = fleet;
  chaos.xidTimes = chaos.xidTimes.filter(t => now - t < CHAOS.xidWindow);
  if (!hasFaultRoom(fleet) || !isDue(chaos.last.xid, now, CHAOS.xidInterval) || chaos.xidTimes.length >= CHAOS.maxXidPerWindow) return null;
  const targets = [...fleet.nodes.values()].filter(n => n.state === "active" && AGENT_NODES.has(n.id) && !isPending(fleet, n.id) && !n.xid);
  if (!targets.length) return null;
  const id = pick(fleet, targets).id, gpu = Math.floor(fleet.rng() * 8), code = pick(fleet, XID_CODES);
  return {
    kind: "xid", text: `xid ${code} on ${id} gpu ${gpu}`,
    run: () => { injectXid(fleet, id, gpu, code); chaos.xidTimes.push(now); chaos.pending.set(id, now); return "ok"; },
  };
}

function planImplant(fleet) {
  if (!hasFaultRoom(fleet) || !isDue(fleet.chaos.last.implant, fleet.now, CHAOS.implantInterval)) return null;
  const inActive = new Set(fleet.reservations.filter(r => r.status === "active").flatMap(r => r.nodes));
  const targets = [...fleet.nodes.values()]
    .filter(n => n.state === "active" && !AGENT_NODES.has(n.id) && inActive.has(n.id) && !isPending(fleet, n.id) && !n.tamper);
  if (!targets.length) return null;
  const id = pick(fleet, targets).id, component = pick(fleet, IMPLANTS);
  return {
    kind: "implant", text: `implant ${component} on ${id}`,
    run: () => { plantImplant(fleet, id, component); fleet.chaos.pending.set(id, fleet.now); return "ok"; },
  };
}

function planReserve(fleet) {
  if (fleet.reservations.filter(isOpen).length >= CHAOS.minOpen) return null;
  const policy = pick(fleet, POLICIES);
  const count = CHAOS.minCount + Math.floor(fleet.rng() * (CHAOS.maxCount - CHAOS.minCount + 1));
  const minutes = CHAOS.minMinutes + Math.floor(fleet.rng() * (CHAOS.maxMinutes - CHAOS.minMinutes + 1));
  const tenant = "tenant-" + ++fleet.chaos.tenants;
  return {
    kind: "reserve", text: `reserve ${count} nodes ${policy} for ${tenant} for ${minutes}m0s`,
    run: () => {
      const placed = reserve(fleet, { tenant, count, policy, minutes });
      return placed.ok ? "ok" : `no capacity, largest block ${placed.largest}`;
    },
  };
}

function hasFaultRoom(fleet) {
  const faulted = new Set([...fleet.nodes.values()].filter(n => n.state === "quarantined").map(n => n.id));
  for (const id of fleet.chaos.pending.keys()) if (isPending(fleet, id)) faulted.add(id);
  return faulted.size < CHAOS.maxFaulted;
}

const isPending = (fleet, id) => fleet.chaos.pending.has(id) && fleet.now - fleet.chaos.pending.get(id) < CHAOS.pendingTTL;
const isDue = (last, now, interval) => last === undefined || now - last >= interval;
const pick = (fleet, list) => list[Math.floor(fleet.rng() * list.length)];

// --- telemetry and records --------------------------------------------------

function sample(fleet) {
  const states = Object.fromEntries(STATES.map(s => [s, 0]));
  const free = { p1: 0, p2: 0 };
  for (const n of fleet.nodes.values()) {
    states[n.state]++;
    if (n.state === "available") free[n.pod]++;
  }
  const reservations = { active: 0, degraded: 0, ended: 0 };
  for (const r of fleet.reservations) reservations[r.status]++;
  fleet.history.push({ t: fleet.now, states, free, reservations });
  if (fleet.history.length > HISTORY) fleet.history.shift();
}

function attestation(fleet, id, reasons) {
  return {
    node_id: id, verdict: reasons.length ? "fail" : "pass", reasons, nonce: hex(fleet, 32),
    measurements_digest: hex(fleet, 64), reference_version: REFERENCE_VERSION,
    attested_at: iso(fleet.now), signature: base64(fleet, 64),
  };
}

export const iso = ms => new Date(ms).toISOString().replace(".000Z", "Z");

function hex(fleet, len) {
  let s = "";
  for (let i = 0; i < len; i++) s += Math.floor(fleet.ids() * 16).toString(16);
  return s;
}

function base64(fleet, bytes) {
  return btoa(String.fromCharCode(...Array.from({ length: bytes }, () => Math.floor(fleet.ids() * 256))));
}

function uuid(fleet) {
  const h = hex(fleet, 32);
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-4${h.slice(13, 16)}-a${h.slice(17, 20)}-${h.slice(20)}`;
}

function mulberry32(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
