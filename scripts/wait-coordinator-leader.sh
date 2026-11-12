#!/usr/bin/env bash
# Wait until the compose stack's coordinator holds the shard-fsm-leader lease.
#
# CI brings the stack up, sleeps, and brings it up again, which recreates the
# coordinator. The first coordinator's lease can outlive it, and until it
# expires (one lease TTL) the new coordinator is a follower: it runs no shard
# FSMs and drains nothing, so a test that starts in that window times out.
#
# Usage: scripts/wait-coordinator-leader.sh [docker compose args, e.g. -f a.yml -f b.yml]
set -euo pipefail

compose=(docker compose "$@")

for _ in $(seq 1 60); do
  # A container's hostname is its id's first 12 characters, and the coordinator's
  # lease holder is "<hostname>-<random>".
  cid=$("${compose[@]}" ps -q coordinator | cut -c1-12)
  holder=$("${compose[@]}" exec -T postgres psql -U weft -d weft -tAc \
    "SELECT holder FROM leases WHERE name = 'shard-fsm-leader' AND expires_at > clock_timestamp()" 2>/dev/null || true)
  if [[ -n "$cid" && "$holder" == "$cid"-* ]]; then
    echo "coordinator $cid holds shard-fsm-leader"
    exit 0
  fi
  sleep 2
done

echo "coordinator never took the shard-fsm-leader lease (container '${cid:-}', holder '${holder:-}')" >&2
"${compose[@]}" logs --tail=50 coordinator >&2 || true
exit 1
