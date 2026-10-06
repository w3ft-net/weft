#!/bin/bash
# Tests for deploy/export-role.sh's pure functions. Run via `make test-deploy`.
# Returns non-zero on first failure so CI can flag.

set -uo pipefail

LIB="$(dirname "$0")/export-role.sh"
PASS=0
FAIL=0

assert_eq() {
    local name="$1" want="$2" got="$3"
    if [ "$want" = "$got" ]; then
        PASS=$((PASS + 1))
        printf "  ok %s\n" "$name"
    else
        FAIL=$((FAIL + 1))
        printf "  FAIL %s\n    want: %q\n     got: %q\n" "$name" "$want" "$got"
    fi
}

section() { printf "%s\n" "── $1 ──"; }

# shellcheck source=export-role.sh
. "$LIB"

section "export_env_contents: happy path"
got=$(export_env_contents "shard-20260921T0000-4a672d0d" 3 "s3://weft-cold/warp")
want=$'EXPORT_SHARD_ID=shard-20260921T0000-4a672d0d\nEXPORT_ATTEMPT=3\nEXPORT_PREFIX=s3://weft-cold/warp'
assert_eq "renders the three variables" "$want" "$got"

section "export_env_contents: rejects bad input"
for case_ in \
    "||" \
    "|3|s3://b/p" \
    "s-1||s3://b/p" \
    "s-1|3|" \
    "s-1|0|s3://b/p" \
    "s-1|-2|s3://b/p" \
    "s-1|abc|s3://b/p" \
    "s 1|3|s3://b/p" \
    "s-1|3|s3://b/p with space" \
    "s-1|3|s3://b/p\"quote" \
    'x$(id)|3|s3://b/p' \
    "s-1|3|s3://b/p;rm" ; do
    IFS='|' read -r shard attempt prefix <<<"$case_"
    export_env_contents "$shard" "$attempt" "$prefix" >/dev/null 2>&1
    assert_eq "rejects [$case_]" "1" "$?"
done

section "export_wait_states"
for st in activating active; do
    export_unit_running_state "$st"
    assert_eq "$st counts as running" "0" "$?"
done
for st in inactive failed deactivating unknown ""; do
    export_unit_running_state "$st"
    assert_eq "[$st] does not count as running" "1" "$?"
done

printf "\n%d passed, %d failed\n" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
