description = "Grant or revoke a comp subscription on a customer (usage: kevcons ext comp grant|revoke|status ...)"

-- Grants and revocations run through `kevcons db tunnel -- psql ...`,
-- which opens an SSM port-forward to RDS, sets PG* env vars, and
-- tears the tunnel down on subprocess exit. Same channel a developer
-- would use for an ad-hoc psql session — the extension only adds the
-- argument shaping and the audit-trail pre-checks.
--
-- See store/subscription.go (GrantCompSubscription, RevokeCompSubscription)
-- for the canonical SQL these queries mirror; the extension reuses the
-- same idempotency rules.

local valid_tiers = { swatch = true, yard = true, bolt = true }

function run(args)
    if #args == 0 then
        usage()
        return
    end

    local action = args[1]
    if action == "grant" then
        run_grant(slice(args, 2))
    elseif action == "revoke" then
        run_revoke(slice(args, 2))
    elseif action == "status" then
        run_status(slice(args, 2))
    else
        kevcons.log("unknown subcommand: " .. action)
        usage()
    end
end

function usage()
    kevcons.log("usage:")
    kevcons.log("  kevcons ext comp grant   --customer NAME --tier swatch|yard|bolt --reason TEXT")
    kevcons.log("  kevcons ext comp revoke  --customer NAME")
    kevcons.log("  kevcons ext comp status  [--customer NAME]")
end

function run_grant(args)
    local opts = parse_flags(args, { "customer", "tier", "reason" })
    if opts.customer == "" or opts.tier == "" or opts.reason == "" then
        kevcons.log("error: grant requires --customer, --tier, and --reason")
        return
    end
    if not valid_tiers[opts.tier] then
        kevcons.log("error: tier must be one of swatch, yard, bolt (got " .. opts.tier .. ")")
        return
    end

    -- Same shape as store.GrantCompSubscription: cancel any active sub,
    -- insert a fresh comp row at the requested tier. Wrapped in a
    -- transaction so a partial failure doesn't strand the customer
    -- between subscriptions.
    local sql = string.format([[
BEGIN;
UPDATE subscriptions
   SET status = 'cancelled', updated_at = now()
 WHERE customer_id = %s AND status = 'active';
INSERT INTO subscriptions
       (id, customer_id, tier, status, starts_at, comp_reason, created_at, updated_at)
VALUES (%s, %s, %s, 'active', now(), %s, now(), now());
COMMIT;
SELECT customer_id, tier, status, comp_reason FROM subscriptions
 WHERE customer_id = %s ORDER BY created_at DESC LIMIT 1;
]],
        sql_lit(opts.customer),
        sql_lit("comp-" .. opts.customer .. "-" .. os.time()),
        sql_lit(opts.customer),
        sql_lit(opts.tier),
        sql_lit(opts.reason),
        sql_lit(opts.customer))

    kevcons.log(string.format("granting %s tier=%s reason=%q",
        opts.customer, opts.tier, opts.reason))
    psql_exec(sql)
    kevcons.log("done. user must re-login for the new tier to flow into JWT claims.")
end

function run_revoke(args)
    local opts = parse_flags(args, { "customer" })
    if opts.customer == "" then
        kevcons.log("error: revoke requires --customer")
        return
    end

    -- Refuse to touch a paid sub: the WHERE-clause guard mirrors
    -- store.RevokeCompSubscription so a typo can't cancel someone
    -- mid-billing-cycle.
    local sql = string.format([[
UPDATE subscriptions
   SET status = 'cancelled', updated_at = now()
 WHERE customer_id = %s AND status = 'active' AND comp_reason != '';
SELECT customer_id, tier, status, comp_reason FROM subscriptions
 WHERE customer_id = %s ORDER BY created_at DESC LIMIT 1;
]],
        sql_lit(opts.customer),
        sql_lit(opts.customer))

    kevcons.log(string.format("revoking comp on %s", opts.customer))
    psql_exec(sql)
end

function run_status(args)
    local opts = parse_flags(args, { "customer" })

    local where = "WHERE comp_reason != ''"
    if opts.customer ~= "" then
        where = string.format("WHERE customer_id = %s ORDER BY created_at DESC", sql_lit(opts.customer))
    else
        where = where .. " AND status = 'active' ORDER BY customer_id, created_at DESC"
    end

    local sql = string.format([[
SELECT customer_id, tier, status, comp_reason, starts_at FROM subscriptions
 %s;
]], where)

    psql_exec(sql)
end

-- ── helpers ──────────────────────────────────────────────────────────

function psql_exec(sql)
    -- kevcons db tunnel -- psql -v ON_ERROR_STOP=1 -c <sql>
    -- The tunnel sets PGHOST/PGPORT/PGUSER/PGDATABASE/PGPASSWORD and
    -- tears down on psql exit.
    local out = kevcons.exec("kevcons", {
        "db", "tunnel", "--",
        "psql", "-v", "ON_ERROR_STOP=1", "-c", sql,
    })
    kevcons.log(out)
end

-- sql_lit single-quotes a string and escapes embedded single quotes.
-- Lightweight because the inputs come from a trusted operator at the
-- shell, not an unverified network source — this is a guard against
-- a stray apostrophe in a reason string, not against attack.
function sql_lit(s)
    return "'" .. (s:gsub("'", "''")) .. "'"
end

function parse_flags(args, allowed)
    local out = {}
    for _, name in ipairs(allowed) do out[name] = "" end
    local i = 1
    while i <= #args do
        local flag = args[i]
        local name = flag:match("^%-%-(.+)$")
        if not name then
            kevcons.log("unexpected argument: " .. flag)
            i = i + 1
        elseif out[name] == nil then
            kevcons.log("unknown flag: --" .. name)
            i = i + 1
        elseif i + 1 > #args then
            kevcons.log("flag --" .. name .. " requires a value")
            i = i + 1
        else
            out[name] = args[i + 1]
            i = i + 2
        end
    end
    return out
end

function slice(t, from)
    local out = {}
    for i = from, #t do
        table.insert(out, t[i])
    end
    return out
end
