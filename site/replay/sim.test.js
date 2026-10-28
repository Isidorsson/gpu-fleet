import { describe, expect, test } from "bun:test";
import {
  AGENT_NODES, TICK_SECONDS, advance, createFleet, injectXid, iso, plantImplant, repairNode, reserve, tick,
} from "./sim.js";

const ticksFor = seconds => Math.ceil(seconds / TICK_SECONDS);
const run = (fleet, step, n) => { for (let i = 0; i < n; i++) step(fleet); };
const node = (fleet, id) => fleet.nodes.get(id);

function activeReservation(fleet, policy, count) {
  const placed = reserve(fleet, { tenant: "acme", count, policy, minutes: 5 });
  if (!placed.ok) throw new Error("reservation did not fit");
  run(fleet, advance, ticksFor(20));
  return placed.reservation;
}

describe("topology", () => {
  test("50 nodes, 8 per rack, r1-r4 in p1 and r5-r7 in p2, all available", () => {
    const fleet = createFleet(1);
    expect(fleet.nodes.size).toBe(50);
    expect(node(fleet, "node-001")).toMatchObject({ pod: "p1", rack: "r1" });
    expect(node(fleet, "node-032")).toMatchObject({ pod: "p1", rack: "r4" });
    expect(node(fleet, "node-033")).toMatchObject({ pod: "p2", rack: "r5" });
    expect(node(fleet, "node-050")).toMatchObject({ pod: "p2", rack: "r7" });
    expect([...fleet.nodes.values()].every(n => n.state === "available")).toBe(true);
  });

  test("every node starts with a passing attestation", () => {
    const fleet = createFleet(1);
    expect(fleet.attestations.get("node-017")).toMatchObject({ node_id: "node-017", verdict: "pass", reasons: [] });
  });
});

describe("determinism", () => {
  test("the same seed tells the same story", () => {
    const a = createFleet(7), b = createFleet(7);
    run(a, tick, 300); run(b, tick, 300);
    expect(a.log).toEqual(b.log);
    expect(a.remediations).toEqual(b.remediations);
  });

  test("a different seed tells a different one", () => {
    const a = createFleet(7), b = createFleet(8);
    run(a, tick, 300); run(b, tick, 300);
    expect(a.log).not.toEqual(b.log);
  });
});

describe("placement", () => {
  test("same_rack keeps the reservation in one rack and provisions it", () => {
    const fleet = createFleet(1);
    const placed = reserve(fleet, { tenant: "acme", count: 6, policy: "same_rack", minutes: 5 });
    expect(placed.ok).toBe(true);
    const nodes = placed.reservation.nodes.map(id => node(fleet, id));
    expect(new Set(nodes.map(n => n.rack)).size).toBe(1);
    expect(nodes.every(n => n.state === "provisioning" && n.tenant === "acme")).toBe(true);
    run(fleet, advance, ticksFor(20));
    expect(nodes.every(n => n.state === "active")).toBe(true);
  });

  test("best fit leaves whole racks free", () => {
    const fleet = createFleet(1);
    const placed = reserve(fleet, { tenant: "acme", count: 2, policy: "any", minutes: 5 });
    expect(placed.reservation.nodes).toEqual(["node-049", "node-050"]);
  });

  test("a request bigger than any rack fails with the largest block", () => {
    const fleet = createFleet(1);
    const placed = reserve(fleet, { tenant: "acme", count: 9, policy: "same_rack", minutes: 5 });
    expect(placed).toEqual({ ok: false, largest: 8 });
    expect(fleet.counters.placementFailures.same_rack).toBe(1);
  });

  test("ending a reservation wipes, re-inspects and frees its nodes", () => {
    const fleet = createFleet(1);
    const res = activeReservation(fleet, "same_pod", 4);
    run(fleet, advance, ticksFor(5 * 60 + 30));
    expect(res.status).toBe("ended");
    expect(res.nodes.every(id => node(fleet, id).state === "available" && !node(fleet, id).tenant)).toBe(true);
  });
});

describe("remediation", () => {
  test("a fatal XID quarantines the node after 88 s and a same-pod replacement follows 2 s later", () => {
    const fleet = createFleet(1);
    const res = activeReservation(fleet, "same_rack", 8);
    const victim = res.nodes.find(id => AGENT_NODES.has(id));
    const faultAt = fleet.now;
    expect(injectXid(fleet, victim, 3, 79)).toBe(true);
    expect(node(fleet, victim).xid).toEqual({ gpu: 3, code: 79, at: faultAt });

    run(fleet, advance, ticksFor(88) - 1);
    expect(node(fleet, victim).state).toBe("active");
    advance(fleet);

    expect(node(fleet, victim).state).toBe("quarantined");
    expect(fleet.remediations[0]).toMatchObject({
      node_id: victim, alertname: "GPUFatalXid", action: "quarantine", outcome: "quarantined",
      at: iso(faultAt + 88_000),
    });
    expect(res.status).toBe("degraded");
    expect(res.nodes).not.toContain(victim);
    const replacement = node(fleet, res.nodes.find(id => node(fleet, id).state === "provisioning"));
    expect(replacement.pod).toBe(node(fleet, victim).pod);
    expect(replacement.events.at(-1)).toMatchObject({ event: "provision", at: iso(faultAt + 90_000) });
  });

  test("an XID on a node without an agent is ignored", () => {
    const fleet = createFleet(1);
    const res = activeReservation(fleet, "same_rack", 8);
    expect(injectXid(fleet, res.nodes.find(id => !AGENT_NODES.has(id)), 0, 79)).toBe(false);
  });

  test("more than 5 quarantines in 10 minutes are rate limited", () => {
    const fleet = createFleet(1);
    for (let i = 0; i < 6; i++) reserve(fleet, { tenant: `t${i}`, count: 8, policy: "same_rack", minutes: 30 });
    run(fleet, advance, ticksFor(20));
    const victims = ["node-001", "node-009", "node-017", "node-025", "node-033", "node-041"];
    for (const id of victims) expect(injectXid(fleet, id, 0, 48)).toBe(true);
    run(fleet, advance, ticksFor(90));
    expect(fleet.remediations.map(r => r.outcome).sort()).toEqual([...Array(5).fill("quarantined"), "rate_limited"].sort());
  });
});

describe("attestation", () => {
  test("an implant survives the wipe but fails attestation, then repair returns the node", () => {
    const fleet = createFleet(1);
    const res = activeReservation(fleet, "same_rack", 4);
    const victim = res.nodes[1];
    plantImplant(fleet, victim, "kernel");
    run(fleet, advance, ticksFor(5 * 60 + 20 + 30));

    expect(node(fleet, victim).state).toBe("quarantined");
    expect(node(fleet, victim).events.at(-1)).toMatchObject({ from: "inspecting", event: "attestation_failed", to: "quarantined" });
    expect(fleet.attestations.get(victim)).toMatchObject({ verdict: "fail", reasons: ["kernel digest not in reference values"] });
    expect(fleet.counters.attestations.fail).toBe(1);

    repairNode(fleet, victim);
    run(fleet, advance, ticksFor(20 + 30));
    expect(node(fleet, victim).state).toBe("available");
    expect(fleet.attestations.get(victim).verdict).toBe("pass");
  });

  test.each([
    ["gpu_vbios", /^gpu \d: vbios 96\.00\.9F\.00\.07 not allowed$/],
    ["cc_mode", /^gpu \d: confidential computing mode off$/],
    ["bmc", /^bmc digest not in reference values$/],
  ])("a %s implant gives the verifier's reason", (component, reason) => {
    const fleet = createFleet(1);
    const res = activeReservation(fleet, "same_rack", 4);
    plantImplant(fleet, res.nodes[0], component);
    run(fleet, advance, ticksFor(5 * 60 + 20 + 30));
    expect(fleet.attestations.get(res.nodes[0]).reasons).toEqual([expect.stringMatching(reason)]);
  });
});

describe("chaos", () => {
  const fleet = createFleet(1);
  const faultedPerTick = [];
  for (let i = 0; i < 2000; i++) {
    tick(fleet);
    faultedPerTick.push([...fleet.nodes.values()].filter(n => n.state === "quarantined").length);
  }
  const nodes = [...fleet.nodes.values()];

  test("tenants hold exactly the provisioning and active nodes", () => {
    for (const n of nodes) expect(Boolean(n.tenant)).toBe(n.state === "provisioning" || n.state === "active");
  });

  test("no node is in two open reservations", () => {
    const open = fleet.reservations.filter(r => r.status === "active" || r.status === "degraded");
    const ids = open.flatMap(r => r.nodes);
    expect(new Set(ids).size).toBe(ids.length);
  });

  test("never more than 3 nodes quarantined at once", () => {
    expect(Math.max(...faultedPerTick)).toBeLessThanOrEqual(3);
  });

  test("the fleet breaks and heals: XIDs, failed attestations and repairs all happen", () => {
    const kinds = new Set(fleet.log.map(l => l.kind));
    expect([...kinds].sort()).toEqual(["implant", "repair", "reserve", "xid"]);
    expect(fleet.remediations.some(r => r.outcome === "quarantined")).toBe(true);
    expect(fleet.counters.attestations.fail).toBeGreaterThan(0);
  });

  test("history keeps the last 30 minutes", () => {
    expect(fleet.history.length).toBe(ticksFor(30 * 60));
    expect(fleet.history.at(-1).t).toBe(fleet.now);
  });
});
