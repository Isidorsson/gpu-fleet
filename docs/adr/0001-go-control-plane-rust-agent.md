# 1. Go control plane, Rust node agent, one repo

Date: 2026-10-19
Status: accepted

## Context

gpu-fleet manages bare-metal GPU nodes: lifecycle (provision, wipe,
inspect), capacity reservations and health monitoring. There are two very
different runtime environments:

- A central control plane: HTTP APIs, Postgres, BMC (Redfish) clients,
  Kubernetes integration. Mostly network I/O and concurrency.
- A per-node agent: runs on every GPU host next to customer workloads,
  reads NVML, exposes metrics. Must be small, predictable and hard to crash.

## Decision

- Control plane in **Go**. The infrastructure ecosystem (client-go,
  controller-runtime, Terraform plugin SDK, Redfish libraries) is Go first,
  and goroutines fit fan-out to hundreds of BMCs.
- Node agent in **Rust**. No GC pauses on a host running latency-sensitive
  training jobs, a single static binary, and `unsafe` confined to the NVML
  FFI boundary (forbidden elsewhere via crate lints).
- One repository. Both sides change together while the API between them is
  still moving, and one CI pipeline keeps them consistent.

## Consequences

- Two toolchains in CI and for contributors.
- The agent-to-control-plane contract must be explicit (Prometheus metrics
  now, a versioned API later) since types cannot be shared across languages.
- Hardware is abstracted behind a backend trait/interface with a fake
  implementation, so everything runs and is tested without GPUs or BMCs.
