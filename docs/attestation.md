# Tenant attestation

Bare-metal tenants have root on the machine. A previous tenant could have
flashed firmware or planted something that survives a disk wipe. Before a
node is handed to the next tenant, the control plane collects evidence of
what the node booted, verifies it against known-good reference values, and
publishes a signed result the tenant can check offline.

This document is the contract between the simulated attester (bmcsim), the
verifier, the control plane service, the provisioner and the API. Each is
built independently against it.

## Roles (IETF RATS, RFC 9334)

| Role | Here | On real hardware |
|---|---|---|
| Attester | bmcsim's in-band deploy agent | the inspection ramdisk running `TPM2_Quote` and NVIDIA GPU attestation (SPDM) |
| Verifier | `internal/attest` (pure) + `internal/attestsvc` | Keylime, NVIDIA NRAS |
| Relying party | the tenant, via the API | the customer's security team |

**This is a simulation.** Keys are Ed25519 held in software, not in a TPM.
Every simulated piece maps one-to-one to a real mechanism, listed in the
last section.

## Threat model

In scope: a previous tenant modifying host firmware, the BMC firmware, the
bootloader, the inspection kernel or GPU VBIOS; disabling GPU confidential
computing mode; replaying old evidence; editing the event log after the fact;
swapping a node's attestation key.

Out of scope (next steps): a real TPM or swtpm, attesting the BMC itself
over its own channel, CPU memory encryption (SEV-SNP, TDX), physical attacks,
a compromised control plane.

## Identity and enrolment

Each node has an attestation key (AK). Its public key is recorded when the
node is racked (`POST /v1/nodes` field `ak_pub`), which stands in for
recording the TPM EK certificate in the asset database at racking time.
After enrolment the AK is never accepted from the node again.

Each GPU has a device key certified by the GPU vendor root, the same shape as
NVIDIA's device certificate chain.

## Simulator key derivation (bmcsim, deterministic from `--seed`)

```
ak_seed(node)          = sha256("gpu-fleet/bmcsim/ak/" + seed + "/" + node_id)
vendor_root_seed       = sha256("gpu-fleet/bmcsim/vendor-root/" + seed)
gpu_seed(node, index)  = sha256("gpu-fleet/bmcsim/gpu/" + seed + "/" + node_id + "/" + index)
```

`seed` is the decimal `--seed` value. Each 32-byte result is an Ed25519 seed
(`ed25519.NewKeyFromSeed`). Each node has 8 GPUs, indices 0 to 7.

## Measurements

Each boot extends three PCRs. All PCRs start as 32 zero bytes and are extended
as `pcr = sha256(pcr || digest)`, in event log order.

| PCR | Component | Simulator default `name` |
|---|---|---|
| 0 | `firmware` | `bios-2.4.1` |
| 1 | `bmc` | `bmc-1.9.3` |
| 4 | `bootloader` | `grub-2.12` |
| 4 | `kernel` | `inspect-ramdisk-6.8.0` |

The event log has exactly these four events, in the table order.

```
digest(name)                  = sha256("gpu-fleet/component/" + name)
tampered_digest(component)    = sha256("gpu-fleet/tampered/" + component)
```

A tampered component reports `name` = `"unknown"` and `tampered_digest`.

GPU defaults: VBIOS `96.00.9F.00.01`, confidential computing mode `on`.
A tampered `gpu_vbios` reports VBIOS `99.99.99.99.99`.

## Signed messages (exact bytes; `\n` is a single newline, hex is lowercase)

```
quote      = "gpu-fleet-quote-v1\n" + node_id + "\n" + nonce_hex + "\n"
             + for each PCR in ascending index: index + ":" + pcr_hex + "\n"
gpu_cert   = "gpu-fleet-gpu-cert-v1\n" + node_id + "\n" + index + "\n" + device_pub_b64 + "\n"
gpu_report = "gpu-fleet-gpu-report-v1\n" + node_id + "\n" + nonce_hex + "\n" + index + "\n" + vbios + "\n" + cc_mode + "\n"
result     = "gpu-fleet-attestation-result-v1\n" + node_id + "\n" + verdict + "\n" + nonce_hex + "\n"
             + measurements_digest_hex + "\n" + reference_version + "\n" + attested_at_rfc3339 + "\n"
             + for each reason in order: reason + "\n"
```

Base64 is standard encoding with padding. `measurements_digest` is
`sha256` over the PCR values (raw bytes) in ascending index order.
`internal/attest` owns these encoders; every component uses them.

## Deploy agent endpoints (bmcsim; basic auth like the rest of bmc-protocol.md)

### `GET /agent/{id}/identity`

`{"ak_pub": "<b64>"}`. Used by `deploy/seed.sh` at racking time only.

### `GET /agent/{id}/attestation?nonce=<64 hex chars>`

Only while the node is powered on (the ramdisk is running); 409 when off.
Bad nonce format: 400.

```json
{
  "node_id": "node-001",
  "nonce": "<hex>",
  "pcrs": {"0": "<hex>", "1": "<hex>", "4": "<hex>"},
  "event_log": [{"pcr": 0, "component": "firmware", "name": "bios-2.4.1", "sha256": "<hex>"}],
  "quote_sig": "<b64, AK over quote>",
  "gpus": [{"index": 0, "vbios": "96.00.9F.00.01", "cc_mode": "on",
            "device_pub": "<b64>", "device_cert_sig": "<b64, vendor root over gpu_cert>",
            "report_sig": "<b64, device key over gpu_report>"}]
}
```

### Simulator-only

- `POST /sim/systems/{id}/tamper {"component": "firmware|bmc|bootloader|kernel|gpu_vbios|cc_mode|ak"}`:
  persistent until cleared, survives power cycles and wipes, like an implant.
  `ak` makes the node sign quotes with a different key; `cc_mode` makes every
  GPU report confidential computing mode `off`.
- `DELETE /sim/systems/{id}/tamper` clears all tampering (a reflash). 204.
- `GET /sim/vendor-root`: `{"vendor_root_pub": "<b64>"}`.

## Reference values (`deploy/attestation/reference.json`)

```json
{
  "version": "2026-10-19.1",
  "components": {
    "firmware":   [{"name": "bios-2.4.1", "sha256": "<hex>"}],
    "bmc":        [{"name": "bmc-1.9.3", "sha256": "<hex>"}],
    "bootloader": [{"name": "grub-2.12", "sha256": "<hex>"}],
    "kernel":     [{"name": "inspect-ramdisk-6.8.0", "sha256": "<hex>"}]
  },
  "gpu": {"vendor_root_pub": "<b64>", "vbios": ["96.00.9F.00.01"], "require_cc_mode": true}
}
```

`require_cc_mode` must be present (no default), so a policy file cannot
silently disable the check by omission. PCR indices in evidence must be
canonical decimal (`"0"`, not `"00"`).

Each component may list several allowed entries, so a firmware rollout adds
the new version before nodes boot it and removes the old one afterwards.

## Verification (`internal/attest.Verify`, pure)

Inputs: evidence, the enrolled AK public key, the expected node ID and nonce,
and the reference values. Checks, all run so that every failure is reported:

1. `node_id` and `nonce` equal the expected values.
2. `quote_sig` verifies with the enrolled AK over `quote`.
3. Replaying `event_log` gives exactly the quoted PCRs, and the log has the
   four required events in order. This is what makes the unsigned log
   trustworthy.
4. Each event's `sha256` is in the reference list for its component.
5. Exactly 8 GPUs, indices 0 to 7. For each: `device_cert_sig` verifies with
   the vendor root, `report_sig` verifies with `device_pub`, VBIOS is allowed,
   and `cc_mode` is `on` when required.

Result: `verdict` `pass` or `fail`, `reasons` (stable strings such as
`bootloader digest not in reference values`, sorted), `measurements_digest`,
`reference_version`.

## Control plane (`internal/attestsvc`)

`func (s *Service) Attest(ctx context.Context, n node.Node) (attest.Result, error)`:
issue a nonce (32 random bytes, single use, 60s TTL, stored), fetch evidence
from the node's deploy agent, consume the nonce, verify, sign the result with
the control plane key, record it. Transport failures wrap
`deployagent.ErrTransient` so the provisioner retries them. A verification
failure is a `fail` result, not an error.

The signing key comes from `ATTEST_SIGNING_KEY` (base64 Ed25519 seed),
required at startup.

## Lifecycle

New event `attestation_failed`: `inspecting -> quarantined`. The provisioner
attests every node after its inspect task succeeds and before powering it
off. `pass` leads to `inspection_passed`; `fail` leads to `attestation_failed`.
So a node only becomes `available` after passing inspection and attestation,
which includes every return to the pool after a wipe.

## API

| Method | Path | |
|---|---|---|
| `GET` | `/v1/nodes/{id}/attestation` | latest signed result: `node_id, verdict, reasons, nonce, measurements_digest, reference_version, attested_at, signature`; 404 if none |
| `GET` | `/v1/attestation/keys` | `{"keys": [{"alg": "Ed25519", "public_key": "<b64>"}]}` |

`POST /v1/nodes` gains required `ak_pub` (base64, 32 bytes decoded).

## Metrics

`attestations_total{verdict}` counter (`pass`, `fail`, pre-initialized to 0).
Alert `AttestationFailure`: `increase(attestations_total{verdict="fail"}[15m]) > 0`, warning.
Dashboard: "Attestation failures (24h)" stat in the Fleet row.

## Mapping to real hardware

| Simulated | Real |
|---|---|
| AK Ed25519 key, `quote` | TPM 2.0 AK (RSA/ECC), `TPM2_Quote` over a PCR selection |
| enrolment via `ak_pub` | EK certificate + `TPM2_MakeCredential`/`ActivateCredential` |
| event log | TCG PC Client firmware event log, IMA |
| `digest(name)` reference values | vendor reference integrity manifests (RIMs), CoRIM |
| vendor root, device cert, `gpu_report` | NVIDIA device identity certificate chain, SPDM measurement report |
| `cc_mode` | H100 confidential computing mode |
| signed `result` | an EAT / EAR attestation result token |
