import { describe, expect, test } from "bun:test";
import { handle } from "./api.js";
import { advance, createFleet, reserve } from "./sim.js";

const get = (fleet, url) => handle(fleet, "GET", url);

describe("read-only API", () => {
  test("writes are refused like the public listener", () => {
    for (const method of ["POST", "PUT", "PATCH", "DELETE"]) {
      expect(handle(createFleet(1), method, "/v1/reservations")).toEqual({
        status: 405, type: "json", body: { error: "this is a read-only public endpoint" },
      });
    }
  });

  test("unknown paths get the Go mux 404", () => {
    expect(get(createFleet(1), "/v2/nodes")).toEqual({ status: 404, type: "text", body: "404 page not found\n" });
  });

  test("capacity lists free nodes and the largest free rack per pod", () => {
    const fleet = createFleet(1);
    reserve(fleet, { tenant: "acme", count: 8, policy: "same_rack", minutes: 5 });
    reserve(fleet, { tenant: "beta", count: 3, policy: "any", minutes: 5 });
    expect(get(fleet, "/v1/capacity").body).toEqual({
      pods: [{ pod: "p1", free: 23, largest_rack: 8 }, { pod: "p2", free: 16, largest_rack: 8 }],
      total_free: 39,
    });
  });

  test("nodes filter by state and carry the tenant only while held", () => {
    const fleet = createFleet(1);
    reserve(fleet, { tenant: "acme", count: 2, policy: "any", minutes: 5 });
    const { nodes } = get(fleet, "/v1/nodes?state=provisioning").body;
    expect(nodes.map(n => n.id)).toEqual(["node-049", "node-050"]);
    expect(nodes[0]).toMatchObject({ pod: "p2", rack: "r7", state: "provisioning", tenant: "acme" });
    expect(Object.keys(nodes[0])).toEqual(["id", "pod", "rack", "bmc", "ak_pub", "state", "tenant"]);
    expect(get(fleet, "/v1/nodes/node-001").body).not.toHaveProperty("tenant");
  });

  test("a node's events, attestation and 404s", () => {
    const fleet = createFleet(1);
    reserve(fleet, { tenant: "acme", count: 2, policy: "any", minutes: 5 });
    advance(fleet); advance(fleet);
    expect(get(fleet, "/v1/nodes/node-050/events").body.events.map(e => e.event))
      .toEqual(["inspection_passed", "provision", "provision_succeeded"]);
    expect(get(fleet, "/v1/nodes/node-050/attestation").body).toMatchObject({ node_id: "node-050", verdict: "pass" });
    expect(get(fleet, "/v1/nodes/node-099")).toMatchObject({ status: 404, body: { error: "node not found" } });
    expect(get(fleet, "/v1/nodes/node-099/attestation")).toMatchObject({ status: 404, body: { error: "attestation not found" } });
  });

  test("reservations filter by tenant and look up by id", () => {
    const fleet = createFleet(1);
    const { reservation } = reserve(fleet, { tenant: "acme", count: 2, policy: "any", minutes: 5 });
    reserve(fleet, { tenant: "beta", count: 2, policy: "any", minutes: 5 });
    const mine = get(fleet, "/v1/reservations?tenant=acme").body.reservations;
    expect(mine).toHaveLength(1);
    expect(Object.keys(mine[0])).toEqual(["id", "tenant", "count", "policy", "starts_at", "ends_at", "status", "nodes", "created_at"]);
    expect(get(fleet, `/v1/reservations/${reservation.id}`).body.id).toBe(reservation.id);
    expect(get(fleet, "/v1/reservations/nope")).toMatchObject({ status: 404, body: { error: "reservation not found" } });
  });

  test("remediations honour the limit bounds", () => {
    const fleet = createFleet(1);
    expect(get(fleet, "/v1/remediations").body).toEqual({ remediations: [] });
    for (const limit of ["0", "501", "x"]) {
      expect(get(fleet, `/v1/remediations?limit=${limit}`)).toEqual({
        status: 400, type: "json", body: { error: "bad request: limit must be an integer from 1 to 500" },
      });
    }
  });

  test("the attestation key and metrics endpoints answer", () => {
    const fleet = createFleet(1);
    expect(get(fleet, "/v1/attestation/keys").body.keys[0]).toMatchObject({ alg: "Ed25519" });
    const metrics = get(fleet, "/metrics");
    expect(metrics.type).toBe("text");
    expect(metrics.body).toContain('fleet_nodes{state="available"} 50');
    expect(metrics.body).toContain('capacity_free_nodes{pod="p2"} 18');
  });
});
