description = "Show EC2 instances for all weft ASGs, grouped by role"

-- ASG definitions: name -> role
local asgs = {
    { name = "weft-coordinator",  role = "coordinator" },
    { name = "weft-shard-small",  role = "shard" },
    { name = "weft-shard-medium", role = "shard" },
    { name = "weft-shard-large",  role = "shard" },
    { name = "weft-compute",      role = "compute" },
}

-- Role display order
local role_order = { "coordinator", "shard", "compute" }

-- ── formatting helpers ─────────────────────────────────────────────────────

local function pad_right(s, width)
    s = tostring(s)
    while #s < width do s = s .. " " end
    return s
end

-- Parse an ISO-8601 timestamp (2024-01-15T12:34:56+00:00 or ...Z) into a Unix
-- timestamp using the `date` CLI, which is available on macOS and Linux.
-- Returns nil on failure.
local function parse_iso8601(ts)
    if not ts then return nil end
    -- Replace trailing Z with +00:00 for broader compatibility
    ts = ts:gsub("Z$", "+00:00")
    -- macOS date: date -j -f format string +%s
    -- GNU date:   date -d string +%s
    -- Try GNU date first (Linux/EC2), fall back to macOS
    local result = io.popen("date -d '" .. ts .. "' +%s 2>/dev/null || date -j -f '%Y-%m-%dT%H:%M:%S%z' '" .. ts:gsub("%+00:00$", "+0000") .. "' +%s 2>/dev/null")
    if not result then return nil end
    local epoch = result:read("*l")
    result:close()
    if epoch and epoch ~= "" then
        return tonumber(epoch)
    end
    return nil
end

-- Format a duration in seconds as a human-readable string: "2d 3h 14m" etc.
local function format_uptime(seconds)
    if not seconds or seconds < 0 then return "unknown" end
    seconds = math.floor(seconds)
    local days    = math.floor(seconds / 86400)
    local hours   = math.floor((seconds % 86400) / 3600)
    local minutes = math.floor((seconds % 3600) / 60)
    if days > 0 then
        return days .. "d " .. hours .. "h " .. minutes .. "m"
    elseif hours > 0 then
        return hours .. "h " .. minutes .. "m"
    else
        return minutes .. "m"
    end
end

-- ── AWS helpers ────────────────────────────────────────────────────────────

-- Fetch ASG details for a list of ASG names in one API call.
-- Returns a map: asg_name -> { desired, min, max, instance_ids }
local function describe_asgs(asg_names)
    local result = {}
    if #asg_names == 0 then return result end

    -- Build a single --auto-scaling-group-names value (space-joined won't work,
    -- but AWS CLI accepts repeated values or comma-sep in some modes).
    -- kevcons.aws() passes args to `aws` CLI, so we pass each name as a separate arg.
    local args = { "autoscaling", "describe-auto-scaling-groups", "--auto-scaling-group-names" }
    for _, name in ipairs(asg_names) do
        args[#args + 1] = name
    end

    local ok, data = pcall(kevcons.aws, unpack(args))
    if not ok or not data then
        kevcons.log("warning: could not fetch ASG details: " .. tostring(data))
        return result
    end

    local groups = data["AutoScalingGroups"]
    if not groups then return result end

    for i = 1, #groups do
        local g = groups[i]
        local name = g["AutoScalingGroupName"]
        local instance_ids = {}
        local instances_raw = g["Instances"]
        if instances_raw then
            for j = 1, #instances_raw do
                local inst = instances_raw[j]
                -- Only include instances that are InService or Pending
                local lc = inst["LifecycleState"] or ""
                if lc ~= "Terminating" and lc ~= "Terminated" then
                    table.insert(instance_ids, inst["InstanceId"])
                end
            end
        end
        result[name] = {
            desired      = g["DesiredCapacity"] or 0,
            min          = g["MinSize"] or 0,
            max          = g["MaxSize"] or 0,
            instance_ids = instance_ids,
        }
    end

    return result
end

-- Fetch EC2 instance details for a list of instance IDs.
-- Returns a map: instance_id -> { state, private_ip, instance_type, launch_time }
local function describe_instances(instance_ids)
    local result = {}
    if #instance_ids == 0 then return result end

    local args = { "ec2", "describe-instances", "--instance-ids" }
    for _, id in ipairs(instance_ids) do
        args[#args + 1] = id
    end

    local ok, data = pcall(kevcons.aws, unpack(args))
    if not ok or not data then
        kevcons.log("warning: could not fetch instance details: " .. tostring(data))
        return result
    end

    local reservations = data["Reservations"]
    if not reservations then return result end

    for i = 1, #reservations do
        local res = reservations[i]
        local instances = res["Instances"]
        if instances then
            for j = 1, #instances do
                local inst = instances[j]
                local id = inst["InstanceId"]
                result[id] = {
                    state         = (inst["State"] and inst["State"]["Name"]) or "unknown",
                    private_ip    = inst["PrivateIpAddress"] or "-",
                    instance_type = inst["InstanceType"] or "-",
                    launch_time   = inst["LaunchTime"],
                }
            end
        end
    end

    return result
end

-- ── rendering ──────────────────────────────────────────────────────────────

-- These are defined inside run() to avoid executing string.rep at load time
-- (the description-extraction VM only loads the base library).

local function print_instance_row(id, info, now)
    local uptime = "unknown"
    if info.launch_time then
        local epoch = parse_iso8601(info.launch_time)
        if epoch then
            uptime = format_uptime(now - epoch)
        end
    end

    kevcons.log(
        "  " ..
        pad_right(id,               22) ..
        pad_right(info.private_ip,  17) ..
        pad_right(info.state,       12) ..
        pad_right(uptime,           14) ..
        info.instance_type
    )
end

local function print_asg_block(asg_name, asg_data, ec2_map, now)
    local count = #asg_data.instance_ids
    local capacity = string.format(
        "desired=%d  min=%d  max=%d",
        asg_data.desired, asg_data.min, asg_data.max
    )

    kevcons.log("")
    kevcons.log("  ASG: " .. asg_name .. "  [" .. capacity .. "]  instances: " .. count)

    if count == 0 then
        kevcons.log("    (no instances)")
        return
    end

    -- Column header
    kevcons.log(
        "  " ..
        pad_right("INSTANCE ID",  22) ..
        pad_right("PRIVATE IP",   17) ..
        pad_right("STATE",        12) ..
        pad_right("UPTIME",       14) ..
        "TYPE"
    )
    kevcons.log("  " .. string.rep("-", 74))

    for _, iid in ipairs(asg_data.instance_ids) do
        local info = ec2_map[iid]
        if info then
            print_instance_row(iid, info, now)
        else
            kevcons.log("  " .. iid .. "  (details unavailable)")
        end
    end
end

-- ── main ───────────────────────────────────────────────────────────────────

function run(args)
    local HEADER_LINE = string.rep("─", 78)
    local THIN_LINE   = string.rep("·", 78)

    kevcons.log(HEADER_LINE)
    kevcons.log("  weft infrastructure status  —  account " .. kevcons.account_id .. "  —  " .. kevcons.region)
    kevcons.log(HEADER_LINE)

    -- Get current time for uptime calculation
    local now = os.time()

    -- Collect all ASG names and fetch in one API call
    local asg_names = {}
    for _, def in ipairs(asgs) do
        table.insert(asg_names, def.name)
    end

    local asg_map = describe_asgs(asg_names)

    -- Collect all instance IDs across all ASGs
    local all_instance_ids = {}
    for _, def in ipairs(asgs) do
        local asg_data = asg_map[def.name]
        if asg_data then
            for _, iid in ipairs(asg_data.instance_ids) do
                table.insert(all_instance_ids, iid)
            end
        end
    end

    -- Fetch EC2 details for all instances in one API call
    local ec2_map = describe_instances(all_instance_ids)

    -- Group ASG definitions by role, preserving role_order
    local asgs_by_role = {}
    for _, def in ipairs(asgs) do
        if not asgs_by_role[def.role] then
            asgs_by_role[def.role] = {}
        end
        table.insert(asgs_by_role[def.role], def.name)
    end

    -- Print each role section
    local total_instances = 0
    for _, role in ipairs(role_order) do
        local role_asgs = asgs_by_role[role]
        if not role_asgs then goto continue end

        -- Count instances in this role
        local role_count = 0
        for _, asg_name in ipairs(role_asgs) do
            local asg_data = asg_map[asg_name]
            if asg_data then
                role_count = role_count + #asg_data.instance_ids
            end
        end
        total_instances = total_instances + role_count

        kevcons.log("")
        kevcons.log(string.upper(role) .. "  (" .. role_count .. " instance" .. (role_count == 1 and "" or "s") .. ")")
        kevcons.log(THIN_LINE)

        for _, asg_name in ipairs(role_asgs) do
            local asg_data = asg_map[asg_name]
            if not asg_data then
                kevcons.log("")
                kevcons.log("  ASG: " .. asg_name .. "  [not found in AWS]")
            else
                print_asg_block(asg_name, asg_data, ec2_map, now)
            end
        end

        ::continue::
    end

    -- Summary footer
    kevcons.log("")
    kevcons.log(HEADER_LINE)
    kevcons.log(string.format("  total: %d instance%s across %d ASGs",
        total_instances,
        total_instances == 1 and "" or "s",
        #asgs))
    kevcons.log(HEADER_LINE)
end
