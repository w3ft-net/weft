#!/bin/bash
# Merge per-container coverage profiles plus optional unit-test counters,
# emit textfmt profiles, and surface totals (raw + core-only). When run
# in CI ($GITHUB_STEP_SUMMARY set), totals also land at the top of the
# job page. Optional threshold gate via $WEFT_COVERAGE_MIN.
#
# Layout produced under coverage/:
#   coverage/<service>/             — raw counters from each container
#   coverage/unit/                  — raw counters from `go test -cover`
#                                     (optional; populated by ci.yml)
#   coverage/_merged/               — `covdata merge` of every input dir
#   coverage/<service>.out          — per-service textfmt
#   coverage/merged.out             — merged textfmt (raw)
#   coverage/core.out               — merged textfmt minus excluded packages
#   coverage/summary.md             — markdown summary
#
# Core excludes generated code (`proto/weftpb/`, `*_gen.go`), vendored
# deps (`frontend/node_modules/`), and CLI entry points (`cmd/`). Gate
# applies to the core number.
set -euo pipefail

cd "$(dirname "$0")/.."

COVDIR="coverage"
MOD="github.com/kevlarcon/weft"
# Regex of paths excluded from the core profile. Matched against
# textfmt block lines (which are prefixed with the full import path).
EXCLUDE_RE="^${MOD//\//\\/}/(proto/weftpb|frontend/node_modules|cmd)/|_gen\.go:"

if [ ! -d "$COVDIR" ]; then
    echo "$COVDIR/ not found — did you run the cover-overlay compose?"
    exit 1
fi

# Discover input dirs that actually contain raw covmeta files. Empty
# service dirs (instrumentation didn't load, or container crashed) are
# skipped so they don't break covdata.
INPUTS=()
for d in "$COVDIR"/*/; do
    [ -d "$d" ] || continue
    name="$(basename "$d")"
    [ "$name" = "_merged" ] && continue
    if compgen -G "$d/covmeta.*" > /dev/null; then
        INPUTS+=("$name")
    fi
done
if [ ${#INPUTS[@]} -eq 0 ]; then
    echo "no coverage data found in $COVDIR/*/"
    exit 1
fi

echo "=== per-service textfmt ==="
for s in "${INPUTS[@]}"; do
    out="$COVDIR/$s.out"
    go tool covdata textfmt -i "$COVDIR/$s" -o "$out"
    echo "  $s: $(wc -l < "$out") profile lines → $out"
done

echo "=== merging counters ==="
merged_dir="$COVDIR/_merged"
rm -rf "$merged_dir" && mkdir -p "$merged_dir"
# covdata merge takes a comma-separated list of input dirs.
inputs_csv=$(IFS=,; echo "${INPUTS[*]/#/$COVDIR/}")
go tool covdata merge -i "$inputs_csv" -o "$merged_dir"
go tool covdata textfmt -i "$merged_dir" -o "$COVDIR/merged.out"
echo "  merged → $COVDIR/merged.out"

echo "=== filtering for core ==="
# Keep the `mode:` header, then drop excluded blocks.
head -n 1 "$COVDIR/merged.out" > "$COVDIR/core.out"
grep -E -v "$EXCLUDE_RE" "$COVDIR/merged.out" | tail -n +2 >> "$COVDIR/core.out" || true
echo "  core → $COVDIR/core.out ($(($(wc -l < "$COVDIR/core.out") - 1)) blocks)"

raw_pct=$(go tool cover -func="$COVDIR/merged.out" | awk '/^total:/ {print $NF}' | tr -d '%')
core_pct=$(go tool cover -func="$COVDIR/core.out"   | awk '/^total:/ {print $NF}' | tr -d '%')

echo
echo "=== totals ==="
printf "  raw  (everything): %s%%\n" "$raw_pct"
printf "  core (gated)     : %s%%\n" "$core_pct"

# Per-package uncovered breakdown over the core profile.
# `go tool cover -func` emits one line per function:
#   <import-path>/<file>:<line>:<col>  <funcname>  <pct>%
# Aggregate by package directory, weight by function count.
top_table=$(go tool cover -func="$COVDIR/core.out" | awk -v mod="$MOD/" '
    !/^total:/ && NF >= 3 {
        path = $1; sub(/:.*/, "", path)
        sub("^" mod, "", path)
        n = split(path, parts, "/")
        pkg = ""
        for (i = 1; i < n; i++) pkg = pkg parts[i] (i < n-1 ? "/" : "")
        if (pkg == "") pkg = "."
        pct = $NF; sub(/%$/, "", pct)
        fns[pkg]++
        sum[pkg] += pct + 0
        if (pct + 0 == 0) uncov[pkg]++
    }
    END {
        for (p in fns) {
            avg = sum[p] / fns[p]
            printf "%6.1f %6d %6d %s\n", avg, uncov[p] + 0, fns[p], p
        }
    }
' | sort -k2,2 -nr | head -10)

# Build the markdown summary.
threshold="${WEFT_COVERAGE_MIN:-0}"
gate_note=""
if [ "$threshold" != "0" ] && [ -n "$threshold" ]; then
    gate_note=" (gate: ≥ ${threshold}%)"
fi

{
    echo "## Coverage"
    echo
    echo "**Core coverage:** ${core_pct}%${gate_note}"
    echo
    echo "| profile | pct |"
    echo "|---|---:|"
    echo "| merged (raw)  | ${raw_pct}% |"
    echo "| merged (core) | **${core_pct}%** |"
    echo
    echo "_Core_ excludes generated code (\`proto/weftpb/\`, \`*_gen.go\`),"
    echo "vendored deps (\`frontend/node_modules/\`), and CLI entry points"
    echo "(\`cmd/\`). The gate runs against the core number."
    echo
    echo "### Top 10 uncovered core packages (by zero-coverage function count)"
    echo
    echo "| avg pct | uncov fns | total fns | package |"
    echo "|---:|---:|---:|---|"
    while IFS= read -r line; do
        # line: "pct uncov fns pkg"
        read -r p u t pkg <<<"$line"
        echo "| ${p}% | ${u} | ${t} | \`${pkg}\` |"
    done <<<"$top_table"
} > "$COVDIR/summary.md"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    cat "$COVDIR/summary.md" >> "$GITHUB_STEP_SUMMARY"
fi
echo
echo "=== summary ==="
cat "$COVDIR/summary.md"

# Gate.
if [ "$threshold" != "0" ] && [ -n "$threshold" ]; then
    # bash can't compare floats; use awk.
    if awk -v c="$core_pct" -v t="$threshold" 'BEGIN { exit !(c+0 < t+0) }'; then
        echo
        echo "coverage-report: core coverage ${core_pct}% < threshold ${threshold}%" >&2
        exit 1
    fi
fi
