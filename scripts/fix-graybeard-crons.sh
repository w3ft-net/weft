#!/usr/bin/env bash
# One-shot repair of graybeard's broken cron watches, via the weft API.
#
# Two classes of breakage (see incident 2026-06-06):
#   1. weft.* -> weft::* bash-name rename (commit 778f704) was never
#      migrated into saved watch pipelines -> "command not found".
#   2. bare-service log paths ("coordinator") now require a kind segment
#      ("coordinator/main") after the ns/svc/kind pseudofile layout change.
#
# Idempotent: each PUT just re-saves schedule+pipeline. Safe to re-run.
set -euo pipefail

API="${WEFT_API:-https://api.w3ft.net}"
TOKEN="$(grep -E '^[[:space:]]*token:' "$HOME/.weft.yaml" | head -1 | sed 's/.*token:[[:space:]]*//;s/["'\'']//g')"

put() {  # name  schedule  pipeline
  local name="$1" schedule="$2" pipeline="$3"
  local code
  code=$(curl -s -o /tmp/cronfix.out -w '%{http_code}' -X PUT "$API/api/watches/$name" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    --data "$(jq -n --arg s "$schedule" --arg p "$pipeline" '{schedule:$s,pipeline:$p}')")
  printf '%-32s -> HTTP %s\n' "$name" "$code"
  [ "$code" = "200" ] || cat /tmp/cronfix.out
}

# --- Class 1: weft. -> weft:: (8 watches) ---
put christen-failure-rate-trend  '0 * * * *'    'weft::rate christen-failure-rate 1h | alert above 0.5 --notify=weft-page'
put coord-restart-loop           '*/5 * * * *'  'weft::delta coord-uptime 10m | alert below 0 --notify=weft-page'
put compute-host-loss            '*/15 * * * *' 'weft::delta compute-host-count 1h | alert below 0 --notify=weft-info'
put shard-host-loss              '*/15 * * * *' 'weft::delta shard-host-count 1h | alert below 0 --notify=weft-page'
put disk-fill-rate               '0 * * * *'    'weft::slope coord-disk-pct 24h | alert above 0.0001 --notify=weft-info'
put coord-fd-leak-trend          '*/15 * * * *' 'weft::slope coord-fds 6h | alert above 0.02 --notify=weft-info'
put coord-goroutine-leak-trend   '*/15 * * * *' 'weft::slope coord-goroutines 6h | alert above 0.05 --notify=weft-info'
put coord-heap-leak-trend        '*/15 * * * *' 'weft::slope coord-heap-mb 6h | alert above 0.001 --notify=weft-info'

# --- Class 2: coordinator -> coordinator/main (4 watches) ---
put christen-failure-rate '*/10 * * * *' "log-pattern-count coordinator/main 2000 'no LAUNCHING row'"
put christen-failures     '*/5 * * * *'  "log-pattern-count coordinator/main 1000 'no LAUNCHING row' | alert above 5 --notify=page"
put compute-exit-127      '*/5 * * * *'  "log-pattern-count coordinator/main 500 'compute exit 127' | alert above 0 --notify=weft-info"
put stream-errors         '*/5 * * * *'  "log-pattern-count coordinator/main 5000 'stream error' | alert above 10 --notify=weft-info"

echo "done."
