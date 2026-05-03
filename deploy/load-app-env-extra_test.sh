#!/bin/bash
# Tests for deploy/load-app-env-extra.sh. Run via `make test-deploy`
# (added in the Makefile). Returns non-zero on first failure so CI
# can flag.

set -uo pipefail

HOOK="$(dirname "$0")/load-app-env-extra.sh"
PASS=0
FAIL=0

# assert_eq <test name> <expected> <actual>
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

# ── happy path ────────────────────────────────────────────────────
section() { printf "%s\n" "── $1 ──"; }

section "all components present → composes DSN"
out=$(
    DATABASE_HOST=db.example.com \
    DATABASE_NAME=weft \
    DATABASE_USER=weft \
    DATABASE_PASSWORD='shh-secret-pw' \
    "$HOOK"
)
want="DB_DSN='host=db.example.com port=5432 dbname=weft user=weft password=shh-secret-pw sslmode=require'"
assert_eq "default port + sslmode" "$want" "$out"

section "explicit port / sslmode override defaults"
out=$(
    DATABASE_HOST=db.example.com \
    DATABASE_NAME=weft \
    DATABASE_USER=weft \
    DATABASE_PASSWORD=pw \
    DATABASE_PORT=5433 \
    DATABASE_SSLMODE=disable \
    "$HOOK"
)
want="DB_DSN='host=db.example.com port=5433 dbname=weft user=weft password=pw sslmode=disable'"
assert_eq "port=5433 sslmode=disable" "$want" "$out"

# ── missing components → silent no-op ─────────────────────────────
section "any missing component → exit 0, no output"
for missing in HOST NAME USER PASSWORD; do
    out=$(
        export DATABASE_HOST=h DATABASE_NAME=n DATABASE_USER=u DATABASE_PASSWORD=p
        unset "DATABASE_$missing"
        "$HOOK"
    )
    rc=$?
    assert_eq "missing $missing → empty stdout" "" "$out"
    assert_eq "missing $missing → exit 0" "0" "$rc"
done

# ── apostrophe rejection ──────────────────────────────────────────
section "apostrophe in any component → exit 1, no DSN emitted"
out=$(
    DATABASE_HOST=db.example.com \
    DATABASE_NAME=weft \
    DATABASE_USER=weft \
    DATABASE_PASSWORD="bob's-pw" \
    "$HOOK" 2>&1
)
rc=$?
case "$out" in
    *"refusing to compose"*) PASS=$((PASS + 1)); printf "  ok apostrophe → refusal message on stderr\n" ;;
    *) FAIL=$((FAIL + 1)); printf "  FAIL apostrophe → expected refusal message, got: %q\n" "$out" ;;
esac
assert_eq "apostrophe → exit 1" "1" "$rc"

# ── exit code only (no DSN line on stdout when failing) ────────────
section "apostrophe → no DSN on stdout"
stdout=$(
    DATABASE_HOST=db.example.com \
    DATABASE_NAME=weft \
    DATABASE_USER=weft \
    DATABASE_PASSWORD="bob's-pw" \
    "$HOOK" 2>/dev/null
)
assert_eq "apostrophe → empty stdout" "" "$stdout"

# ── summary ───────────────────────────────────────────────────────
printf "\n%d passed, %d failed\n" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
