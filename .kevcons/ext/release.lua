description = "Release weftd or weft CLI binaries to S3 (usage: kevcons ext release weftd|weft)"

local bucket = "weft-releases-220664822695"
local base_url = "https://releases.w3ft.net"

local targets = {
    weftd = {
        pkg         = "./cmd/weftd/",
        version_file = "cmd/weftd/version.go",
        test_pkgs   = "./cmd/weftd/ ./weftd/",
        platforms   = {
            { goos = "linux", goarch = "amd64", suffix = "linux-amd64" },
        },
    },
    weft = {
        pkg         = "./cmd/weft/",
        version_file = "cmd/weft/version.go",
        test_pkgs   = "./cmd/weft/ ./shell/ ./planner/",
        platforms   = {
            { goos = "linux",  goarch = "amd64", suffix = "linux-amd64" },
            { goos = "darwin", goarch = "arm64", suffix = "darwin-arm64" },
        },
    },
}

function run(args)
    if #args == 0 then
        kevcons.log("usage: kevcons ext release weftd|weft")
        return
    end

    local name = args[1]
    local target = targets[name]
    if not target then
        kevcons.log("unknown target: " .. name .. " (expected weftd or weft)")
        return
    end

    -- Read version from version.go
    local version = read_version(target.version_file)
    if not version then
        kevcons.log("could not read version from " .. target.version_file)
        return
    end

    local tag = name .. "/v" .. version
    kevcons.log("releasing " .. name .. " v" .. version)

    -- Check for uncommitted changes
    local status = kevcons.exec("git", {"status", "--porcelain"})
    -- Trim whitespace
    status = status:match("^%s*(.-)%s*$")
    if status ~= "" then
        kevcons.log("error: uncommitted changes in working tree")
        kevcons.log(status)
        return
    end

    -- Check tag doesn't already exist
    local existing = kevcons.exec("git", {"tag", "-l", tag})
    existing = existing:match("^%s*(.-)%s*$")
    if existing ~= "" then
        kevcons.log("error: tag " .. tag .. " already exists — bump version in " .. target.version_file)
        return
    end

    -- Run tests
    kevcons.log("running tests...")
    local test_result = kevcons.exec("go", {"test", unpack(split(target.test_pkgs))})
    kevcons.log(test_result)

    -- Build and upload each platform
    for _, plat in ipairs(target.platforms) do
        local binary_name = name .. "-" .. plat.suffix
        local tmp = "/tmp/" .. binary_name

        kevcons.log("building " .. binary_name .. "...")
        kevcons.exec("go", {"build", "-o", tmp, target.pkg}, {
            env = {
                GOOS = plat.goos,
                GOARCH = plat.goarch,
                CGO_ENABLED = "0",
            },
        })

        -- Upload versioned
        local s3_versioned = "s3://" .. bucket .. "/" .. name .. "/v" .. version .. "/" .. binary_name
        kevcons.log("uploading " .. s3_versioned)
        kevcons.aws_exec("s3", "cp", tmp, s3_versioned)

        -- Upload latest
        local s3_latest = "s3://" .. bucket .. "/" .. name .. "/latest/" .. binary_name
        kevcons.log("uploading " .. s3_latest)
        kevcons.aws_exec("s3", "cp", tmp, s3_latest)

        -- Clean up
        os.remove(tmp)
    end

    -- Tag and push
    kevcons.log("tagging " .. tag)
    kevcons.exec("git", {"tag", tag})
    kevcons.exec("git", {"push", "origin", tag})

    kevcons.log("")
    kevcons.log("released " .. name .. " v" .. version)
    kevcons.log("")
    for _, plat in ipairs(target.platforms) do
        local binary_name = name .. "-" .. plat.suffix
        kevcons.log("  " .. base_url .. "/" .. name .. "/v" .. version .. "/" .. binary_name)
    end
    if name == "weftd" then
        kevcons.log("")
        kevcons.log("update consuming apps: weft.version: v" .. version)
    end
end

function read_version(path)
    local f = io.open(path, "r")
    if not f then return nil end
    local content = f:read("*a")
    f:close()
    return content:match('Version%s*=%s*"([^"]+)"')
end

function split(s)
    local parts = {}
    for part in s:gmatch("%S+") do
        table.insert(parts, part)
    end
    return parts
end
