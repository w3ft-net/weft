description = "Release weftd or weft CLI binaries to S3 (usage: kevcons ext release weftd|weft)"

local bucket = "weft-releases-220664822695"
local base_url = "https://releases.w3ft.net"

local targets = {
    weftd = {
        pkg         = "./cmd/weftd/",
        version_file = "cmd/weftd/version.go",
        test_pkgs   = "./cmd/weftd/ ./weftd/",
        deb         = true,
        platforms   = {
            { goos = "linux", goarch = "amd64", suffix = "linux-amd64", deb_arch = "amd64" },
            { goos = "linux", goarch = "arm64", suffix = "linux-arm64", deb_arch = "arm64" },
        },
    },
    weft = {
        pkg         = "./cmd/weft/",
        version_file = "cmd/weft/version.go",
        test_pkgs   = "./cmd/weft/ ./shell/ ./planner/",
        cgo         = true,
        platforms   = {
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

    -- Skip if this version is already released (check remote tags since
    -- CI shallow clones don't fetch tags locally)
    local remote_tags = kevcons.exec("git", {"ls-remote", "--tags", "origin", "refs/tags/" .. tag})
    remote_tags = remote_tags:match("^%s*(.-)%s*$")
    if remote_tags ~= "" then
        kevcons.log(tag .. " already released, skipping (bump version in " .. target.version_file .. " to release)")
        return
    end

    -- Run tests
    kevcons.log("running tests...")
    local test_result = kevcons.exec("go", {"test", unpack(split(target.test_pkgs))})
    kevcons.log(test_result)

    -- Resolve the current commit SHA (short) so it ends up baked into
    -- each binary via ldflags. The Version is already in version.go;
    -- we pass it via -X main.Version=… so the in-source const remains
    -- the canonical source of truth (a future ldflags-only override
    -- would let an out-of-band tag drift from the file).
    local sha = kevcons.exec("git", {"rev-parse", "--short", "HEAD"})
    sha = sha:match("^%s*(.-)%s*$")
    local ldflags = "-X main.Version=" .. version .. " -X main.GitSHA=" .. sha

    -- per-platform sha256 records, harvested while we already have the
    -- artifact on disk. Used to write manifest.json after the loop.
    local manifest_platforms = {}

    -- Build and upload each platform
    for _, plat in ipairs(target.platforms) do
        local binary_name = name .. "-" .. plat.suffix
        local tmp = "/tmp/" .. binary_name

        kevcons.log("building " .. binary_name .. "...")
        kevcons.exec("go", {"build", "-ldflags", ldflags, "-o", tmp, target.pkg}, {
            env = {
                GOOS = plat.goos,
                GOARCH = plat.goarch,
                CGO_ENABLED = target.cgo and "1" or "0",
            },
        })

        -- sha256 of the freshly-built binary (before any deb wrapping).
        -- Recorded in manifest.json so `weft update` can verify its
        -- download. macOS `shasum`, Linux `sha256sum` — try both.
        local sum = kevcons.exec("sh", {"-c", "shasum -a 256 " .. tmp .. " 2>/dev/null || sha256sum " .. tmp})
        local sha256 = sum:match("^(%x+)")
        table.insert(manifest_platforms, {
            suffix = plat.suffix,
            goos = plat.goos,
            goarch = plat.goarch,
            sha256 = sha256 or "",
        })

        if target.deb and plat.deb_arch then
            -- Build .deb package
            local deb_name = name .. "_" .. version .. "_" .. plat.deb_arch .. ".deb"
            local deb_tmp = "/tmp/" .. deb_name
            local pkg_root = "/tmp/" .. name .. "-deb-" .. plat.deb_arch

            kevcons.log("packaging " .. deb_name .. "...")
            kevcons.exec("rm", {"-rf", pkg_root})
            kevcons.exec("mkdir", {"-p", pkg_root .. "/DEBIAN"})
            kevcons.exec("mkdir", {"-p", pkg_root .. "/usr/local/bin"})
            kevcons.exec("mkdir", {"-p", pkg_root .. "/usr/lib/weftd"})
            kevcons.exec("mkdir", {"-p", pkg_root .. "/etc/default"})
            kevcons.exec("mkdir", {"-p", pkg_root .. "/etc/systemd/system"})

            -- Write control file with actual version and arch
            local ctrl = read_file("deploy/weftd/deb/DEBIAN/control")
            ctrl = ctrl:gsub("{{VERSION}}", version)
            ctrl = ctrl:gsub("amd64", plat.deb_arch)
            write_file(pkg_root .. "/DEBIAN/control", ctrl)

            -- Copy control scripts
            kevcons.exec("cp", {"deploy/weftd/deb/DEBIAN/postinst", pkg_root .. "/DEBIAN/postinst"})
            kevcons.exec("chmod", {"0755", pkg_root .. "/DEBIAN/postinst"})
            kevcons.exec("cp", {"deploy/weftd/deb/DEBIAN/conffiles", pkg_root .. "/DEBIAN/conffiles"})

            -- Place files
            kevcons.exec("cp", {tmp, pkg_root .. "/usr/local/bin/weftd"})
            kevcons.exec("chmod", {"0755", pkg_root .. "/usr/local/bin/weftd"})
            kevcons.exec("cp", {"deploy/weftd/weftd.service", pkg_root .. "/etc/systemd/system/weftd.service"})
            kevcons.exec("cp", {"deploy/weftd/weftd-config.sh", pkg_root .. "/usr/lib/weftd/weftd-config.sh"})
            kevcons.exec("chmod", {"0755", pkg_root .. "/usr/lib/weftd/weftd-config.sh"})
            kevcons.exec("cp", {"deploy/weftd/weftd.default", pkg_root .. "/etc/default/weftd"})
            kevcons.exec("chmod", {"0644", pkg_root .. "/etc/default/weftd"})

            -- Build deb
            kevcons.exec("dpkg-deb", {"--build", pkg_root, deb_tmp})

            -- Upload versioned
            local s3_versioned = "s3://" .. bucket .. "/" .. name .. "/v" .. version .. "/" .. deb_name
            kevcons.log("uploading " .. s3_versioned)
            kevcons.aws_exec("s3", "cp", deb_tmp, s3_versioned)

            -- Upload latest
            local s3_latest = "s3://" .. bucket .. "/" .. name .. "/latest/" .. deb_name
            kevcons.log("uploading " .. s3_latest)
            kevcons.aws_exec("s3", "cp", deb_tmp, s3_latest)

            -- Clean up deb artifacts
            kevcons.exec("rm", {"-rf", pkg_root})
            os.remove(deb_tmp)
        else
            -- Upload raw binary (weft CLI)
            local s3_versioned = "s3://" .. bucket .. "/" .. name .. "/v" .. version .. "/" .. binary_name
            kevcons.log("uploading " .. s3_versioned)
            kevcons.aws_exec("s3", "cp", tmp, s3_versioned)

            local s3_latest = "s3://" .. bucket .. "/" .. name .. "/latest/" .. binary_name
            kevcons.log("uploading " .. s3_latest)
            kevcons.aws_exec("s3", "cp", tmp, s3_latest)
        end

        -- Clean up binary
        os.remove(tmp)
    end

    -- Write manifest.json — consumed by `weft update` (and the future
    -- weftd self-update path) to discover the latest version and verify
    -- downloads. Versioned + latest aliases mirror the binary uploads.
    local manifest_lines = {
        '{',
        '  "binary": "' .. name .. '",',
        '  "version": "' .. version .. '",',
        '  "sha": "' .. sha .. '",',
        '  "platforms": [',
    }
    for i, p in ipairs(manifest_platforms) do
        local sep = i < #manifest_platforms and "," or ""
        local artifact_name = name .. "-" .. p.suffix
        local artifact_url = base_url .. "/" .. name .. "/v" .. version .. "/" .. artifact_name
        table.insert(manifest_lines,
            '    {"suffix":"' .. p.suffix
            .. '","goos":"' .. p.goos
            .. '","goarch":"' .. p.goarch
            .. '","sha256":"' .. p.sha256
            .. '","url":"' .. artifact_url
            .. '"}' .. sep)
    end
    table.insert(manifest_lines, '  ]')
    table.insert(manifest_lines, '}')
    local manifest_body = table.concat(manifest_lines, "\n") .. "\n"
    local manifest_tmp = "/tmp/" .. name .. "-manifest.json"
    write_file(manifest_tmp, manifest_body)

    local s3_versioned_manifest = "s3://" .. bucket .. "/" .. name .. "/v" .. version .. "/manifest.json"
    kevcons.log("uploading " .. s3_versioned_manifest)
    kevcons.aws_exec("s3", "cp", manifest_tmp, s3_versioned_manifest)

    local s3_latest_manifest = "s3://" .. bucket .. "/" .. name .. "/latest/manifest.json"
    kevcons.log("uploading " .. s3_latest_manifest)
    kevcons.aws_exec("s3", "cp", manifest_tmp, s3_latest_manifest)
    os.remove(manifest_tmp)

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

function read_file(path)
    local f = io.open(path, "r")
    if not f then return "" end
    local content = f:read("*a")
    f:close()
    return content
end

function write_file(path, content)
    local f = io.open(path, "w")
    if not f then return end
    f:write(content)
    f:close()
end

function split(s)
    local parts = {}
    for part in s:gmatch("%S+") do
        table.insert(parts, part)
    end
    return parts
end
