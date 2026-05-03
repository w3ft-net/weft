#!/bin/bash
# load-app-env-extra: weft's per-app hook for /usr/local/bin/load-app-env.
#
# After load-app-env has populated DATABASE_USER / DATABASE_PASSWORD
# (from the AWS-managed credentials secret) and DATABASE_HOST /
# DATABASE_NAME (from the SSM config blob), this hook composes a
# libpq-style DSN string and emits it as DB_DSN. weft-coordinator,
# weft-api, and weft-migrate all consume DB_DSN — keeping the
# composition here means none of them need to know how to assemble
# one from the components.
#
# Invoked by load-app-env at the end of its load sequence; the loader
# `set -a; . "$TMP"; set +a`s the partial .env so the components are
# already in the environment when this script runs. Stdout is
# appended verbatim to .env.
#
# Bail silently (exit 0) if any component is missing — the consumers
# fall back to their own defaults (--db-dsn flag or the dev
# localhost DSN) and a missing component on a real instance is a
# Terraform-side problem to surface elsewhere.

set -euo pipefail

host="${DATABASE_HOST:-}"
name="${DATABASE_NAME:-}"
user="${DATABASE_USER:-}"
pass="${DATABASE_PASSWORD:-}"
port="${DATABASE_PORT:-5432}"
ssl="${DATABASE_SSLMODE:-require}"

if [ -z "$host" ] || [ -z "$name" ] || [ -z "$user" ] || [ -z "$pass" ]; then
    exit 0
fi

# load-app-env emits values in single-quoted form; an apostrophe in
# any component would corrupt the bash parse on `set -a; . .env`.
# The credentials module's random_password excludes ' from its
# override_special set, so prod is safe; refuse loudly if a dev or
# test setup violates the rule rather than silently producing a
# corrupt .env.
case "$host$name$user$pass" in
    *\'*)
        echo "load-app-env-extra: DB component contains an apostrophe; refusing to compose DB_DSN" >&2
        exit 1
        ;;
esac

printf "DB_DSN='host=%s port=%s dbname=%s user=%s password=%s sslmode=%s'\n" \
    "$host" "$port" "$name" "$user" "$pass" "$ssl"
