#!/usr/bin/env bash
#
# Read the run-event record for one owner, straight from the control plane.
#
# Why this is a script and not a curl line: the record has no user interface, and the
# assertion it needs can only be minted by town. Notifications are not built yet, so this is
# the only way to see what an agent recorded.
#
# Requirements
#   - the town repository checked out beside this one, because the assertion code lives there
#   - kubectl configured against the cluster
#
# Usage
#   cluster-setup-scripts/read-events.sh [owner]
#   TOWN=~/town OWNER=github#1234 cluster-setup-scripts/read-events.sh
#
# Notes
#   - an ownership read shows you YOUR agents and nobody else's. An empty answer for a
#     workspace you do not own is the isolation working, not a fault.
#   - reading does not mark anything as read. Use POST /api/events/ack, or these rows come
#     back every time.
set -euo pipefail

OWNER="${OWNER:-${1:-github#583231}}"
TOWN="${TOWN:-$HOME/town}"
LOCAL_PORT="${LOCAL_PORT:-18080}"

KEY="$(mktemp)"
trap 'rm -f "$KEY"' EXIT
kubectl -n town get secret town-assertion-key -o jsonpath='{.data.assertion-key\.pem}' \
  | base64 -d > "$KEY"

TOKEN="$(cd "$TOWN" && KEY="$KEY" OWNER="$OWNER" node --input-type=module -e '
import { readFileSync } from "node:fs";
import { Assertions } from "./src/server/assertion.ts";
const a = await Assertions.fromPem(readFileSync(process.env.KEY, "utf8"), "pestilence-api", 7200);
console.log(await a.mint(process.env.OWNER));
' 2>/dev/null)"

kubectl -n pestilence port-forward "svc/pestilence" "$LOCAL_PORT:8080" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true; rm -f "$KEY"' EXIT
sleep 4

curl -sS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$LOCAL_PORT/api/events" \
  | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("cursor=%s sequence=%s count=%d" % (d.get("cursor"), d.get("sequence"), len(d.get("events", []))))
for e in d.get("events", []):
    print("  seq=%-4s %-18s %-22s %s -> %s" % (
        e["sequence"], e["kind"], e["run_id"], e["previous_state"] or "-", e["state"]))
'
