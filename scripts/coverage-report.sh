#!/bin/bash
# Merge per-container coverage profiles from coverage/<service>/ into
# a single textfmt output plus per-service textfmt outputs. Run after
# a coverage-instrumented integration run has finished and the stack
# has been torn down (so all binaries have flushed their counters).
#
#   docker compose -f docker-compose.yml -f docker-compose.cover.yml up -d --build
#   go test -tags integration ./integration/ -count=1 -timeout 5m
#   docker compose -f docker-compose.yml -f docker-compose.cover.yml down
#   scripts/coverage-report.sh
#
# Outputs:
#   coverage/merged.out            — union of every service's counters
#   coverage/<service>.out         — per-service textfmt
set -euo pipefail

cd "$(dirname "$0")/.."

COVDIR="coverage"
if [ ! -d "$COVDIR" ]; then
    echo "$COVDIR/ not found — did you run the cover-overlay compose?"
    exit 1
fi

SERVICES=()
for d in "$COVDIR"/*/; do
    [ -d "$d" ] || continue
    # Skip directories with no counter files — services that didn't
    # get instrumented (or crashed before flushing) land empty here.
    if ! compgen -G "$d/covmeta.*" > /dev/null; then
        continue
    fi
    SERVICES+=("$(basename "$d")")
done

if [ ${#SERVICES[@]} -eq 0 ]; then
    echo "no coverage data found in $COVDIR/*/"
    exit 1
fi

echo "=== per-service coverage ==="
for s in "${SERVICES[@]}"; do
    out="$COVDIR/$s.out"
    go tool covdata textfmt -i "$COVDIR/$s" -o "$out"
    n=$(wc -l < "$out")
    echo "  $s: $n profile lines → $out"
done

echo "=== merged coverage ==="
merged_dir="$COVDIR/_merged"
rm -rf "$merged_dir"
mkdir -p "$merged_dir"

# `go tool covdata merge` takes comma-separated input directories.
inputs=$(IFS=,; echo "${SERVICES[*]/#/$COVDIR/}")
go tool covdata merge -i "$inputs" -o "$merged_dir"

go tool covdata textfmt -i "$merged_dir" -o "$COVDIR/merged.out"
echo "  merged → $COVDIR/merged.out"

echo
echo "=== total merged coverage ==="
go tool cover -func="$COVDIR/merged.out" | tail -1
