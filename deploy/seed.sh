#!/usr/bin/env bash
# Registers every system bmcsim exposes with the control plane.
# Topology: 8 nodes per rack, racks r1..r4 in pod p1, r5 and up in pod p2.
# Usage: BMCSIM_USER=... BMCSIM_PASSWORD=... deploy/seed.sh [bmc_url] [controlplane_url]
set -euo pipefail

NODES_PER_RACK=8
POD1_RACKS=4

bmc="${1:-http://localhost:8443}"
cp="${2:-http://localhost:8080}"
: "${BMCSIM_USER:?set BMCSIM_USER}" "${BMCSIM_PASSWORD:?set BMCSIM_PASSWORD}"

ids=$(curl -fsS --retry 5 --retry-all-errors -u "$BMCSIM_USER:$BMCSIM_PASSWORD" "$bmc/redfish/v1/Systems" \
  | grep -o '"/redfish/v1/Systems/[^"]*"' | sed 's#.*/##; s#"##')

for id in $ids; do
  rack_number=$(( (10#${id##*-} - 1) / NODES_PER_RACK + 1 ))
  rack="r$rack_number"
  pod=p1
  if (( rack_number > POD1_RACKS )); then pod=p2; fi
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$cp/v1/nodes" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$id\",\"pod\":\"$pod\",\"rack\":\"$rack\",\"bmc\":\"$bmc\"}")
  echo "$id $pod $rack -> $code"
done
