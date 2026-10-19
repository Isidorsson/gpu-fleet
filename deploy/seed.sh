#!/usr/bin/env bash
# Registers every system bmcsim exposes with the control plane, enrolling each
# node's attestation key as read from its deploy agent. Reading the key here,
# once, stands in for recording the TPM EK certificate when a node is racked.
# Topology: 8 nodes per rack, racks r1..r4 in pod p1, r5 and up in pod p2.
# Usage: BMCSIM_USER=... BMCSIM_PASSWORD=... deploy/seed.sh [bmc_url] [controlplane_url]
set -euo pipefail

NODES_PER_RACK=8
POD1_RACKS=4

bmc="${1:-http://localhost:8443}"
cp="${2:-http://localhost:8080}"
: "${BMCSIM_USER:?set BMCSIM_USER}" "${BMCSIM_PASSWORD:?set BMCSIM_PASSWORD}"

# bmcsim injects transient 503s, so every read retries.
bmc_get() {
  curl -fsS --retry 5 --retry-all-errors -u "$BMCSIM_USER:$BMCSIM_PASSWORD" "$bmc$1"
}

ak_pub() {
  bmc_get "/agent/$1/identity" | sed -n 's/.*"ak_pub"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

ids=$(bmc_get /redfish/v1/Systems | grep -o '"/redfish/v1/Systems/[^"]*"' | sed 's#.*/##; s#"##')

for id in $ids; do
  rack_number=$(( (10#${id##*-} - 1) / NODES_PER_RACK + 1 ))
  rack="r$rack_number"
  pod=p1
  if (( rack_number > POD1_RACKS )); then pod=p2; fi
  ak=$(ak_pub "$id")
  if [[ -z "$ak" ]]; then
    echo "$id: identity response has no ak_pub" >&2
    exit 1
  fi
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$cp/v1/nodes" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$id\",\"pod\":\"$pod\",\"rack\":\"$rack\",\"bmc\":\"$bmc\",\"ak_pub\":\"$ak\"}")
  echo "$id $pod $rack -> $code"
done
