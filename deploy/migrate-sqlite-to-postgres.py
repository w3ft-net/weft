#!/usr/bin/env python3
"""
One-time migration: dump weft SQLite databases and load into PostgreSQL.

Usage:
    # Copy weft.db and weft-api.db from the coordinator instance first:
    # scp coordinator:/opt/app/data/weft.db /tmp/weft.db
    # scp coordinator:/opt/app/data/weft-api.db /tmp/weft-api.db

    export WEFT_DB_DSN="host=<rds-endpoint> dbname=weft user=weft password=<password> sslmode=require"
    python3 deploy/migrate-sqlite-to-postgres.py /tmp/weft.db /tmp/weft-api.db
"""

import os
import sqlite3
import sys

import psycopg2


def create_schema(pg_conn) -> None:
    """Create tables matching GORM AutoMigrate output for configstore + API."""
    pg = pg_conn.cursor()
    pg.execute("""
        -- configstore tables
        CREATE TABLE IF NOT EXISTS customers (
            id BIGSERIAL PRIMARY KEY,
            created_at TIMESTAMPTZ,
            updated_at TIMESTAMPTZ,
            deleted_at TIMESTAMPTZ,
            name TEXT NOT NULL
        );
        CREATE UNIQUE INDEX IF NOT EXISTS idx_customers_name ON customers(name);
        CREATE INDEX IF NOT EXISTS idx_customers_deleted_at ON customers(deleted_at);

        CREATE TABLE IF NOT EXISTS accounts (
            id BIGSERIAL PRIMARY KEY,
            created_at TIMESTAMPTZ,
            updated_at TIMESTAMPTZ,
            deleted_at TIMESTAMPTZ,
            customer_id BIGINT NOT NULL,
            aws_account TEXT NOT NULL,
            role_arn TEXT NOT NULL,
            external_id TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_accounts_deleted_at ON accounts(deleted_at);

        CREATE TABLE IF NOT EXISTS log_groups (
            id BIGSERIAL PRIMARY KEY,
            created_at TIMESTAMPTZ,
            updated_at TIMESTAMPTZ,
            deleted_at TIMESTAMPTZ,
            account_id BIGINT NOT NULL,
            app TEXT NOT NULL,
            log_group_name TEXT NOT NULL,
            region TEXT NOT NULL DEFAULT 'us-east-2',
            schema_yaml TEXT NOT NULL,
            poll_interval TEXT NOT NULL DEFAULT '30s',
            enabled BOOLEAN NOT NULL DEFAULT TRUE,
            last_poll_at TEXT,
            last_token TEXT
        );
        CREATE UNIQUE INDEX IF NOT EXISTS idx_account_app ON log_groups(account_id, app);
        CREATE INDEX IF NOT EXISTS idx_log_groups_deleted_at ON log_groups(deleted_at);

        CREATE TABLE IF NOT EXISTS agent_tokens (
            id BIGSERIAL PRIMARY KEY,
            created_at TIMESTAMPTZ,
            updated_at TIMESTAMPTZ,
            deleted_at TIMESTAMPTZ,
            customer_id BIGINT NOT NULL,
            token TEXT NOT NULL,
            label TEXT,
            app TEXT,
            revoked BOOLEAN NOT NULL DEFAULT FALSE
        );
        CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tokens_token ON agent_tokens(token);
        CREATE INDEX IF NOT EXISTS idx_agent_tokens_deleted_at ON agent_tokens(deleted_at);

        CREATE TABLE IF NOT EXISTS shard_records (
            id TEXT PRIMARY KEY,
            apps TEXT NOT NULL,
            window_start TEXT NOT NULL,
            window_end TEXT NOT NULL,
            state TEXT NOT NULL,
            addr TEXT,
            instance_id TEXT,
            parquet_path TEXT,
            tier TEXT,
            row_count BIGINT DEFAULT 0,
            created_at TEXT NOT NULL
        );

        -- API tables
        CREATE TABLE IF NOT EXISTS users (
            id TEXT PRIMARY KEY,
            email TEXT NOT NULL,
            name TEXT,
            customer_name TEXT,
            tier TEXT NOT NULL DEFAULT 'trial',
            trial_ends_at TIMESTAMPTZ,
            stripe_customer_id TEXT,
            stripe_subscription_id TEXT,
            created_at TIMESTAMPTZ,
            updated_at TIMESTAMPTZ
        );
        CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email);

        CREATE TABLE IF NOT EXISTS magic_links (
            id BIGSERIAL PRIMARY KEY,
            email TEXT NOT NULL,
            token TEXT NOT NULL,
            expires_at TIMESTAMPTZ,
            used BOOLEAN NOT NULL DEFAULT FALSE
        );
        CREATE UNIQUE INDEX IF NOT EXISTS idx_magic_links_token ON magic_links(token);

        CREATE TABLE IF NOT EXISTS saved_queries (
            id TEXT PRIMARY KEY,
            user_id TEXT NOT NULL,
            name TEXT NOT NULL,
            query TEXT NOT NULL,
            created_at TIMESTAMPTZ,
            updated_at TIMESTAMPTZ
        );
        CREATE INDEX IF NOT EXISTS idx_saved_queries_user_id ON saved_queries(user_id);
    """)
    pg_conn.commit()
    print("  Schema created.")


def migrate_configstore(sqlite_path: str, pg_conn) -> None:
    """Migrate configstore tables: customers, accounts, log_groups, agent_tokens, shard_records."""
    src = sqlite3.connect(sqlite_path)
    src.row_factory = sqlite3.Row
    cur = src.cursor()
    pg = pg_conn.cursor()

    # Customers
    cur.execute("SELECT id, created_at, updated_at, deleted_at, name FROM customers")
    for row in cur:
        pg.execute(
            """INSERT INTO customers (id, created_at, updated_at, deleted_at, name)
               VALUES (%s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            (row["id"], row["created_at"], row["updated_at"], row["deleted_at"], row["name"]),
        )
    print(f"  customers: {cur.rowcount} rows")

    # Accounts
    cur.execute(
        "SELECT id, created_at, updated_at, deleted_at, customer_id, aws_account, role_arn, external_id FROM accounts"
    )
    for row in cur:
        pg.execute(
            """INSERT INTO accounts (id, created_at, updated_at, deleted_at, customer_id, aws_account, role_arn, external_id)
               VALUES (%s, %s, %s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            tuple(row),
        )
    print(f"  accounts: {cur.rowcount} rows")

    # Log groups
    cur.execute(
        """SELECT id, created_at, updated_at, deleted_at, account_id, app,
                  log_group_name, region, schema_yaml, poll_interval, enabled,
                  last_poll_at, last_token
           FROM log_groups"""
    )
    for row in cur:
        vals = list(row)
        vals[10] = bool(vals[10])  # enabled: int → bool
        pg.execute(
            """INSERT INTO log_groups (id, created_at, updated_at, deleted_at, account_id, app,
                  log_group_name, region, schema_yaml, poll_interval, enabled, last_poll_at, last_token)
               VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            tuple(vals),
        )
    print(f"  log_groups: {cur.rowcount} rows")

    # Agent tokens
    cur.execute(
        """SELECT id, created_at, updated_at, deleted_at, customer_id, token, label, app, revoked
           FROM agent_tokens"""
    )
    for row in cur:
        vals = list(row)
        vals[8] = bool(vals[8])  # revoked: int → bool
        pg.execute(
            """INSERT INTO agent_tokens (id, created_at, updated_at, deleted_at, customer_id, token, label, app, revoked)
               VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            tuple(vals),
        )
    print(f"  agent_tokens: {cur.rowcount} rows")

    # Shard records
    cur.execute(
        """SELECT id, apps, window_start, window_end, state, addr, instance_id,
                  parquet_path, tier, row_count, created_at
           FROM shard_records"""
    )
    for row in cur:
        pg.execute(
            """INSERT INTO shard_records (id, apps, window_start, window_end, state, addr, instance_id,
                  parquet_path, tier, row_count, created_at)
               VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            tuple(row),
        )
    print(f"  shard_records: {cur.rowcount} rows")

    pg_conn.commit()
    src.close()


def migrate_api(sqlite_path: str, pg_conn) -> None:
    """Migrate API tables: users, magic_links, saved_queries."""
    src = sqlite3.connect(sqlite_path)
    src.row_factory = sqlite3.Row
    cur = src.cursor()
    pg = pg_conn.cursor()

    # Users
    cur.execute(
        """SELECT id, email, name, customer_name, tier, trial_ends_at,
                  stripe_customer_id, stripe_subscription_id, created_at, updated_at
           FROM users"""
    )
    for row in cur:
        pg.execute(
            """INSERT INTO users (id, email, name, customer_name, tier, trial_ends_at,
                  stripe_customer_id, stripe_subscription_id, created_at, updated_at)
               VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            tuple(row),
        )
    print(f"  users: {cur.rowcount} rows")

    # Saved queries
    cur.execute("SELECT id, user_id, name, query, created_at, updated_at FROM saved_queries")
    for row in cur:
        pg.execute(
            """INSERT INTO saved_queries (id, user_id, name, query, created_at, updated_at)
               VALUES (%s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO NOTHING""",
            tuple(row),
        )
    print(f"  saved_queries: {cur.rowcount} rows")

    # Skip magic_links — ephemeral, no value in migrating expired tokens
    print("  magic_links: skipped (ephemeral)")

    pg_conn.commit()
    src.close()


def main():
    if len(sys.argv) < 2:
        print(f"Usage: {sys.argv[0]} <configstore.db> [api.db]")
        sys.exit(1)

    configstore_db = sys.argv[1]
    api_db = sys.argv[2] if len(sys.argv) > 2 else None
    dsn = os.environ.get("WEFT_DB_DSN")

    if not dsn:
        # Fall back to PG* env vars (set by kevcons db tunnel --)
        pghost = os.environ.get("PGHOST")
        if pghost:
            dsn = (
                f"host={pghost} "
                f"port={os.environ.get('PGPORT', '5432')} "
                f"dbname={os.environ.get('PGDATABASE', 'weft')} "
                f"user={os.environ.get('PGUSER', 'weft')} "
                f"password={os.environ.get('PGPASSWORD', '')} "
                f"sslmode=require"
            )

    if not dsn:
        print("Error: set WEFT_DB_DSN or PG* environment variables")
        print("  Hint: kevcons db tunnel -- python3 deploy/migrate-sqlite-to-postgres.py /tmp/weft.db")
        sys.exit(1)

    print("Connecting to PostgreSQL...")
    conn = psycopg2.connect(dsn)

    print("Creating schema...")
    create_schema(conn)

    print(f"Migrating configstore from {configstore_db}...")
    migrate_configstore(configstore_db, conn)

    if api_db:
        print(f"Migrating API store from {api_db}...")
        migrate_api(api_db, conn)
    else:
        print("No API database provided, skipping.")

    conn.close()
    print("Done.")


if __name__ == "__main__":
    main()
