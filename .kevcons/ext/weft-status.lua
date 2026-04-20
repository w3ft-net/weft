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
-- Returns a map: instance_id -> { state, private_ip, instance_type, launch_time,
--                                  volumes: [ { volume_id, device, attach_state, shard_id } ] }
-- Volumes come from BlockDeviceMappings on the instance; size and the shard_id
-- tag are fetched separately via describe_volumes_for_instances().
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

                -- Collect attached volume IDs from BlockDeviceMappings
                local volumes = {}
                local bdms = inst["BlockDeviceMappings"]
                if bdms then
                    for k = 1, #bdms do
                        local bdm = bdms[k]
                        local ebs = bdm["Ebs"]
                        if ebs and ebs["VolumeId"] then
                            table.insert(volumes, {
                                volume_id    = ebs["VolumeId"],
                                device       = bdm["DeviceName"] or "-",
                                attach_state = ebs["Status"] or "-",
                                -- size and shard_id filled in by describe_volumes_for_instances()
                                size_gb      = nil,
                                shard_id     = nil,
                            })
                        end
                    end
                end

                result[id] = {
                    state         = (inst["State"] and inst["State"]["Name"]) or "unknown",
                    private_ip    = inst["PrivateIpAddress"] or "-",
                    instance_type = inst["InstanceType"] or "-",
                    launch_time   = inst["LaunchTime"],
                    volumes       = volumes,
                }
            end
        end
    end

    return result
end

-- Fetch describe-volumes details (size, tags) for volumes already listed in
-- ec2_map, and also fetch unattached volumes tagged shard_id=pending-* or
-- matching the weft-shard naming pattern.
--
-- Mutates ec2_map in place (fills in size_gb and shard_id on each volume entry).
-- Returns a list of orphan volume records:
--   [ { volume_id, size_gb, shard_id_tag, create_time, state } ]
local function fetch_volume_details(ec2_map)
    -- Gather all volume IDs attached to any known instance
    local attached_vol_ids = {}
    local vol_id_to_instance = {}  -- volume_id -> instance_id (for back-fill)
    local vol_id_to_idx = {}       -- volume_id -> index in instance's volumes list

    for iid, info in pairs(ec2_map) do
        for idx, vol in ipairs(info.volumes) do
            table.insert(attached_vol_ids, vol.volume_id)
            vol_id_to_instance[vol.volume_id] = iid
            vol_id_to_idx[vol.volume_id] = idx
        end
    end

    -- ── 1. Describe attached volumes to get size + shard_id tag ──────────────
    if #attached_vol_ids > 0 then
        local args = { "ec2", "describe-volumes", "--volume-ids" }
        for _, vid in ipairs(attached_vol_ids) do
            args[#args + 1] = vid
        end

        local ok, data = pcall(kevcons.aws, unpack(args))
        if ok and data and data["Volumes"] then
            local vols = data["Volumes"]
            for i = 1, #vols do
                local v = vols[i]
                local vid = v["VolumeId"]

                -- Extract shard_id tag
                local shard_id_tag = nil
                local tags = v["Tags"]
                if tags then
                    for t = 1, #tags do
                        if tags[t]["Key"] == "shard_id" then
                            shard_id_tag = tags[t]["Value"]
                            break
                        end
                    end
                end

                -- Back-fill into ec2_map
                local iid = vol_id_to_instance[vid]
                local idx = vol_id_to_idx[vid]
                if iid and idx then
                    ec2_map[iid].volumes[idx].size_gb  = v["Size"]
                    ec2_map[iid].volumes[idx].shard_id = shard_id_tag
                end
            end
        else
            kevcons.log("warning: could not fetch attached volume details: " .. tostring(data))
        end
    end

    -- ── 2. Find orphan / unattached volumes tagged for weft shards ───────────
    -- Filter: status=available AND (shard_id tag starts with "pending-" OR
    -- shard_id tag matches weft-shard-* pattern).
    -- We use two separate filter calls because OR isn't natively supported in
    -- the CLI filter syntax across different tag values.

    local orphans = {}
    local seen_orphan_ids = {}

    local function fetch_orphans_by_filter(filter_value)
        local args = {
            "ec2", "describe-volumes",
            "--filters",
            "Name=status,Values=available",
            "Name=tag:shard_id,Values=" .. filter_value,
        }
        local ok, data = pcall(kevcons.aws, unpack(args))
        if not ok or not data or not data["Volumes"] then return end

        local vols = data["Volumes"]
        for i = 1, #vols do
            local v = vols[i]
            local vid = v["VolumeId"]
            if not seen_orphan_ids[vid] then
                seen_orphan_ids[vid] = true

                local shard_id_tag = nil
                local tags = v["Tags"]
                if tags then
                    for t = 1, #tags do
                        if tags[t]["Key"] == "shard_id" then
                            shard_id_tag = tags[t]["Value"]
                            break
                        end
                    end
                end

                table.insert(orphans, {
                    volume_id    = vid,
                    size_gb      = v["Size"],
                    shard_id_tag = shard_id_tag or "-",
                    create_time  = v["CreateTime"],
                    state        = v["State"] or "available",
                })
            end
        end
    end

    -- pending-* orphans (instance was terminated before volume was attached)
    fetch_orphans_by_filter("pending-*")
    -- volumes tagged for any weft shard tier
    fetch_orphans_by_filter("weft-shard-*")

    return orphans
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

    -- Print attached volumes indented beneath the instance row
    if #info.volumes == 0 then
        kevcons.log("    volumes: (none attached)")
    else
        for _, vol in ipairs(info.volumes) do
            local size_str = vol.size_gb and (vol.size_gb .. "GB") or "?GB"
            local tag_str  = vol.shard_id and ("shard_id=" .. vol.shard_id) or "shard_id=(untagged)"
            kevcons.log(
                "    vol  " ..
                pad_right(vol.volume_id,    25) ..
                pad_right(vol.device,       12) ..
                pad_right(size_str,          8) ..
                pad_right(vol.attach_state, 12) ..
                tag_str
            )
        end
    end
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

local function print_orphan_volumes(orphans, now)
    kevcons.log("")
    kevcons.log("  ORPHAN / UNATTACHED SHARD VOLUMES  (" .. #orphans .. ")")
    kevcons.log("  " .. string.rep("-", 74))

    if #orphans == 0 then
        kevcons.log("    (none)")
        return
    end

    kevcons.log(
        "  " ..
        pad_right("VOLUME ID",   25) ..
        pad_right("SIZE",         8) ..
        pad_right("STATE",       12) ..
        pad_right("AGE",         14) ..
        "SHARD_ID TAG"
    )
    kevcons.log("  " .. string.rep("-", 74))

    for _, vol in ipairs(orphans) do
        local size_str = vol.size_gb and (vol.size_gb .. "GB") or "?GB"
        local age_str  = "unknown"
        if vol.create_time then
            local epoch = parse_iso8601(vol.create_time)
            if epoch then
                age_str = format_uptime(now - epoch)
            end
        end

        kevcons.log(
            "  " ..
            pad_right(vol.volume_id,    25) ..
            pad_right(size_str,          8) ..
            pad_right(vol.state,        12) ..
            pad_right(age_str,          14) ..
            vol.shard_id_tag
        )
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

    -- Fetch EC2 details (including BlockDeviceMappings) for all instances
    local ec2_map = describe_instances(all_instance_ids)

    -- Fetch volume details: fills in size/tag on attached vols, returns orphans
    local orphan_volumes = fetch_volume_details(ec2_map)

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

        -- Print orphan volumes beneath the shard role section
        if role == "shard" then
            print_orphan_volumes(orphan_volumes, now)
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
