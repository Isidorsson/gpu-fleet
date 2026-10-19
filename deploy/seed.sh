#!/usr/bin/env bash
# Registers every system bmcsim exposes with the control plane.
# Usage: BMCSIM_USER=... BMCSIM_PASSWORD=... deploy/seed.sh [bmc_url] [controlplane_url]
set -euo pipefail

bmc="${1:-http://localhost:8443}"
cp="${2:-http://localhost:8080}"
: "${BMCSIM_USER:?set BMCSIM_USER}" "${BMCSIM_PASSWORD:?set BMCSIM_PASSWORD}"

ids=$(curl -fsS --retry 5 --retry-all-errors -u "$BMCSIM_USER:$BMCSIM_PASSWORD" "$bmc/redfish/v1/Systems" \
  | grep -o '"/redfish/v1/Systems/[^"]*"' | sed 's#.*/##; s#"##')

for id in $ids; do
  rack="r$(( (10#${id##*-} - 1) / 8 + 1 ))"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$cp/v1/nodes" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$id\",\"rack\":\"$rack\",\"bmc\":\"$bmc\"}")
  echo "$id $rack -> $code"
done
