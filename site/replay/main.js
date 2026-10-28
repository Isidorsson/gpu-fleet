// Wires the simulated fleet (sim.js) and its read-only API (api.js) to the
// #fleet-replay section: rack floor, dashboard panels, chaos log and console.

import { handle } from "./api.js";
import { AGENT_NODES, START, TICK_SECONDS, createFleet, tick } from "./sim.js";

const SEED = 19;
const WARMUP_TICKS = 180;                 // start 30 simulated minutes in, mid-story
const SPEEDS = { "10x": 1000, "30x": 333, "90x": 111 };
const AREA_STATES = ["available", "inspecting", "provisioning", "active", "wiping", "quarantined"];
const PRESETS = ["/v1/capacity", "/v1/remediations?limit=5", "/v1/reservations", "/v1/nodes?state=quarantined", "/v1/attestation/keys", "/metrics"];
const SVG = "http://www.w3.org/2000/svg";
const STATUS_TEXT = { 200: "OK", 400: "Bad Request", 404: "Not Found", 405: "Method Not Allowed" };

const root = document.getElementById("fleet-replay");
if (root) mount(root);

function mount(root) {
  const $ = sel => root.querySelector(sel);
  let fleet, timer = null, speed = "30x", inView = false, paused = false;
  const units = buildFloor($("[data-floor]"), id => runRequest(`GET /v1/nodes/${id}/events`));

  function reset() {
    fleet = createFleet(SEED);
    for (let i = 0; i < WARMUP_TICKS; i++) tick(fleet);
    render();
  }

  function schedule() {
    clearInterval(timer);
    const running = inView && !document.hidden && !paused;
    timer = running ? setInterval(() => { tick(fleet); render(); }, SPEEDS[speed]) : null;
    $("[data-play]").textContent = paused ? "Play" : "Pause";
  }

  function render() {
    paintFloor(units, fleet);
    $("[data-clock]").textContent = clock(fleet.now);
    renderStats($("[data-stats]"), fleet);
    renderArea($("[data-area]"), fleet.history);
    renderFree($("[data-free]"), fleet.history.at(-1).free);
    renderRows($("[data-log]"), fleet.log.slice(0, 8), l => [clock(Date.parse(l.at)), l.action, l.outcome]);
    renderRows($("[data-remediations]"), fleet.remediations.slice(0, 5), r => [clock(Date.parse(r.at)), r.node_id, r.outcome]);
    const xids = [...fleet.nodes.values()].filter(n => n.xid);
    renderRows($("[data-xids]"), xids, n => [n.id, `gpu ${n.xid.gpu}`, `XID ${n.xid.code}`]);
  }

  function runRequest(line) {
    const [method, path] = line.trim().includes(" ") ? line.trim().split(/\s+/, 2) : ["GET", line.trim()];
    $("[data-request]").value = `${method.toUpperCase()} ${path}`;
    const res = handle(fleet, method.toUpperCase(), path || "/");
    const body = res.type === "json" ? JSON.stringify(res.body, null, 2) : res.body;
    $("[data-response]").textContent = `HTTP/1.1 ${res.status} ${STATUS_TEXT[res.status]}\n\n${body}`;
  }

  $("[data-play]").addEventListener("click", () => { paused = !paused; schedule(); });
  $("[data-restart]").addEventListener("click", () => { reset(); schedule(); });
  for (const btn of root.querySelectorAll("[data-speed]")) {
    btn.addEventListener("click", () => {
      speed = btn.dataset.speed;
      for (const b of root.querySelectorAll("[data-speed]")) b.setAttribute("aria-pressed", String(b === btn));
      schedule();
    });
  }
  $("[data-console]").addEventListener("submit", e => { e.preventDefault(); runRequest($("[data-request]").value); });
  const presets = $("[data-presets]");
  for (const p of PRESETS) {
    const b = el("button", "preset", p);
    b.type = "button";
    b.addEventListener("click", () => runRequest(`GET ${p}`));
    presets.append(b);
  }

  paused = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  reset();
  runRequest("GET /v1/capacity");
  new IntersectionObserver(([entry]) => { inView = entry.isIntersecting; schedule(); }).observe(root);
  document.addEventListener("visibilitychange", schedule);
}

// --- rack floor -------------------------------------------------------------

function buildFloor(container, onSelect) {
  const units = new Map();
  const pods = { p1: el("div", "pod"), p2: el("div", "pod") };
  for (const [name, pod] of Object.entries(pods)) {
    pod.append(el("div", "label", "pod " + name), el("div", "pod-racks"));
    container.append(pod);
  }
  for (let r = 1; r <= 7; r++) {
    const rack = el("div", "rack");
    rack.append(el("div", "rack-name", "r" + r));
    for (let u = 0; u < 8; u++) {
      const n = (r - 1) * 8 + u + 1;
      if (n > 50) { rack.append(el("div", "unit empty")); continue; }
      const id = "node-" + String(n).padStart(3, "0");
      const unit = el("button", "unit");
      unit.type = "button";
      unit.append(el("span", "", id.slice(5) + (AGENT_NODES.has(id) ? "·a" : "")), el("i", "led"));
      unit.addEventListener("click", () => onSelect(id));
      units.set(id, unit);
      rack.append(unit);
    }
    pods[r <= 4 ? "p1" : "p2"].querySelector(".pod-racks").append(rack);
  }
  return units;
}

function paintFloor(units, fleet) {
  for (const [id, unit] of units) {
    const n = fleet.nodes.get(id);
    unit.dataset.s = n.state;
    unit.classList.toggle("hot", Boolean(n.xid));
    unit.title = `${id} · ${n.state}${n.tenant ? " · " + n.tenant : ""}${n.xid ? ` · XID ${n.xid.code} gpu ${n.xid.gpu}` : ""}`;
    unit.setAttribute("aria-label", unit.title);
  }
}

// --- panels -----------------------------------------------------------------

function renderStats(container, fleet) {
  const nodes = [...fleet.nodes.values()];
  const stats = [
    ["Quarantined", nodes.filter(n => n.state === "quarantined").length, "bad"],
    ["Unhealthy GPUs", nodes.filter(n => n.xid).length, "bad"],
    ["Attestation failures", fleet.counters.attestations.fail, "warn"],
    ["Auto-quarantined", fleet.counters.remediations.quarantined ?? 0, "warn"],
    ["Degraded reservations", fleet.counters.degraded, "warn"],
    ["Free nodes", nodes.filter(n => n.state === "available").length, "ok"],
  ];
  container.replaceChildren(...stats.map(([label, value, tone]) => {
    const tile = el("div", "stat");
    tile.append(el("div", "label", label), el("div", `value ${value ? tone : ""}`, String(value)));
    return tile;
  }));
}

function renderArea(svg, history) {
  const W = 600, H = 160, total = 50;
  const x = i => (i / Math.max(1, history.length - 1)) * W;
  const y = v => H - (v / total) * H;
  const below = history.map(() => 0);
  const paths = AREA_STATES.map(state => {
    const top = history.map((h, i) => below[i] + h.states[state]);
    const d = [
      ...top.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`),
      ...below.map((v, i) => `L${x(i).toFixed(1)},${y(v).toFixed(1)}`).reverse(),
      "Z",
    ].join("");
    top.forEach((v, i) => { below[i] = v; });
    const path = document.createElementNS(SVG, "path");
    path.setAttribute("d", d);
    path.setAttribute("class", "area " + state);
    return path;
  });
  svg.replaceChildren(...paths);
}

function renderFree(container, free) {
  const max = { p1: 32, p2: 18 };
  container.replaceChildren(...Object.entries(free).map(([pod, n]) => {
    const row = el("div", "bar-row");
    const bar = el("div", "bar");
    const fill = el("i");
    fill.style.width = `${(n / max[pod]) * 100}%`;
    bar.append(fill);
    row.append(el("span", "mono", pod), bar, el("span", "mono", `${n}/${max[pod]}`));
    return row;
  }));
}

function renderRows(tbody, items, cells) {
  if (!items.length) {
    const row = el("tr");
    const td = el("td", "empty", "none yet");
    td.colSpan = 3;
    row.append(td);
    return tbody.replaceChildren(row);
  }
  tbody.replaceChildren(...items.map(item => {
    const row = el("tr");
    row.append(...cells(item).map(c => el("td", "", c)));
    return row;
  }));
}

function clock(ms) {
  const elapsed = Math.floor((ms - START) / 1000 / TICK_SECONDS) * TICK_SECONDS;
  return new Date(START + elapsed * 1000).toISOString().slice(11, 19);
}

function el(tag, cls = "", text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}
