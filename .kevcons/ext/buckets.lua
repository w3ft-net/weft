description = "Manage weft shard buckets (usage: kevcons ext buckets list|get|set)"

-- ── helpers ────────────────────────────────────────────────────────────────

local function json_unescape(s)
    s = s:gsub("\\u(%x%x%x%x)", function(hex)
        return string.char(tonumber(hex, 16))
    end)
    s = s:gsub("\\\\", "\0")
    s = s:gsub('\\"', '"')
    s = s:gsub("\\n", "\n")
    s = s:gsub("\\t", "\t")
    s = s:gsub("\\/", "/")
    s = s:gsub("\\b", "\b")
    s = s:gsub("\\f", "\f")
    s = s:gsub("\\r", "\r")
    s = s:gsub("\0", "\\")
    return s
end

local function sql_lit(s)
    return "'" .. (s:gsub("'", "''")) .. "'"
end

local function table_contains(t, v)
    for _, x in ipairs(t) do
        if x == v then return true end
    end
    return false
end

local function parse_flags(args, allowed)
    local out = {}
    local i = 1
    while i <= #args do
        local flag = args[i]
        local name = flag:match("^%-%-(.+)$")
        if not name then
            kevcons.log("unexpected argument: " .. flag)
            i = i + 1
        elseif not table_contains(allowed, name) then
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

-- ── DB connection ─────────────────────────────────────────────────────────

local db_conn = nil -- lazily established on first psql_exec

local function connect_db()
    if db_conn then return db_conn end

    -- 1. Discover RDS endpoint
    kevcons.log("discovering RDS instance...")
    local rds = kevcons.aws("rds", "describe-db-instances",
        "--db-instance-identifier", kevcons.app_name .. "-postgres")
    local inst = rds.DBInstances[1]
    local host = inst.Endpoint.Address
    local port = tostring(math.floor(inst.Endpoint.Port))
    local dbname = inst.DBName
    local user = inst.MasterUsername

    -- 2. Retrieve password from Secrets Manager
    kevcons.log("retrieving credentials...")
    local secret = kevcons.aws("secretsmanager", "get-secret-value",
        "--secret-id", kevcons.app_name .. "-db-credentials")
    local creds = secret.SecretString
    local password = json_unescape(
        creds:match('"[Pp]assword"%s*:%s*"([^"]*)"') or creds)

    -- 3. Find an SSM-connected relay instance
    kevcons.log("finding SSM relay instance...")
    local asg_name = kevcons.app_name .. "-coordinator"
    local asg = kevcons.aws("autoscaling", "describe-auto-scaling-groups",
        "--auto-scaling-group-names", asg_name)
    local group = asg.AutoScalingGroups[1]
    local instance_ids = {}
    for _, inst in ipairs(group.Instances) do
        if inst.HealthStatus == "Healthy" and inst.LifecycleState == "InService" then
            table.insert(instance_ids, inst.InstanceId)
        end
    end
    if #instance_ids == 0 then
        error("no healthy instances in ASG " .. asg_name)
    end

    -- Pick the first healthy instance as relay (SSM check via tunnel will
    -- fail fast if it's not connected)
    local relay_id = instance_ids[1]

    -- 4. Open tunnel
    kevcons.log("opening tunnel via " .. relay_id .. "...")
    local tunnel = kevcons.tunnel(relay_id, host, port)
    kevcons.log("tunnel ready on localhost:" .. tostring(tunnel.port))

    db_conn = {
        tunnel = tunnel,
        env = {
            PGHOST = "127.0.0.1",
            PGPORT = tostring(math.floor(tunnel.port)),
            PGUSER = user,
            PGDATABASE = dbname,
            PGPASSWORD = password,
            PGSSLMODE = "require",
        },
    }
    return db_conn
end

local function psql_exec(sql)
    local conn = connect_db()
    local out = kevcons.exec("psql", {
        "-v", "ON_ERROR_STOP=1",
        "--pset=pager=off",
        "-c", sql,
    }, { env = conn.env })
    kevcons.log(out)
end

-- ── subcommands ────────────────────────────────────────────────────────────

local function run_list()
    psql_exec([[
SELECT id,
       label,
       apps,
       target_ingesting_count,
       tier,
       window_duration,
       window_duration
  FROM buckets
 ORDER BY id;
]])
end

local function run_get(args)
    local opts = parse_flags(args, { "label" })
    if not opts.label or opts.label == "" then
        kevcons.log("error: get requires --label <name>")
        kevcons.log("usage: kevcons ext buckets get --label <name>")
        return
    end

    psql_exec(string.format([[
SELECT id,
       label,
       apps,
       target_ingesting_count,
       tier,
       window_duration,
       created_at
  FROM buckets
 WHERE label = %s;
]], sql_lit(opts.label)))
end

local function run_set(args)
    local opts = parse_flags(args, { "label", "window-duration", "target", "tier" })

    if not opts.label or opts.label == "" then
        kevcons.log("error: set requires --label <name>")
        kevcons.log("usage: kevcons ext buckets set --label <name> [--window-duration <dur>] [--target <n>] [--tier <tier>]")
        return
    end

    local assignments = {}

    if opts["window-duration"] then
        table.insert(assignments, "window_duration = " .. sql_lit(opts["window-duration"]))
    end
    if opts["tier"] then
        table.insert(assignments, "tier = " .. sql_lit(opts["tier"]))
    end
    if opts["target"] then
        local n = tonumber(opts["target"])
        if not n or n ~= math.floor(n) or n < 0 then
            kevcons.log("error: --target must be a non-negative integer (got " .. opts["target"] .. ")")
            return
        end
        table.insert(assignments, "target_ingesting_count = " .. tostring(math.floor(n)))
    end

    if #assignments == 0 then
        kevcons.log("error: set requires at least one field to update")
        kevcons.log("  --window-duration <dur>    e.g. 168h, 24h")
        kevcons.log("  --target <n>               target_ingesting_count")
        kevcons.log("  --tier <tier>              e.g. small, medium, large")
        return
    end

    local set_clause = table.concat(assignments, ",\n       ")

    kevcons.log("updating bucket " .. opts.label .. "...")
    psql_exec(string.format([[
BEGIN;
UPDATE buckets
   SET %s
 WHERE label = %s;
SELECT id,
       label,
       apps,
       target_ingesting_count,
       tier,
       window_duration
  FROM buckets
 WHERE label = %s;
COMMIT;
]], set_clause, sql_lit(opts.label), sql_lit(opts.label)))
end

local function run_reset_window(args)
    local opts = parse_flags(args, { "duration" })
    local dur = opts["duration"] or "1 hour"

    kevcons.log("resetting INGESTING shard window_end to next " .. dur .. " boundary...")
    psql_exec(string.format([[
UPDATE shard_records
   SET window_end = to_char(
         date_trunc('hour', now() AT TIME ZONE 'UTC') + interval '%s',
         'YYYY-MM-DD"T"HH24:MI:SS"Z"')
 WHERE state = 'INGESTING'
RETURNING id, state, window_start, window_end;
]], dur))
end

-- ── entry point ────────────────────────────────────────────────────────────

function run(args)
    if #args == 0 then
        usage()
        return
    end

    local subcmd = args[1]
    local rest = {}
    for i = 2, #args do rest[#rest + 1] = args[i] end

    if subcmd == "list" then
        run_list()
    elseif subcmd == "get" then
        run_get(rest)
    elseif subcmd == "set" then
        run_set(rest)
    elseif subcmd == "reset-window" then
        run_reset_window(rest)
    else
        kevcons.log("unknown subcommand: " .. subcmd)
        usage()
    end
end

function usage()
    kevcons.log("usage:")
    kevcons.log("  kevcons ext buckets list")
    kevcons.log("  kevcons ext buckets get  --label <name>")
    kevcons.log("  kevcons ext buckets set  --label <name> [--window-duration <dur>] [--target <n>] [--tier <tier>]")
    kevcons.log("  kevcons ext buckets reset-window [--duration <interval>]   (default: '1 hour')")
end
