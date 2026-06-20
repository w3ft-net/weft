description = "Build and deploy a weft Lambda binary (usage: kevcons ext release-lambda [cold-shard|compactor], default cold-shard)"

-- Per-target build config. Both build from the same Dockerfile.lambda; the
-- compactor selects its entrypoint via the CMD_PATH build arg.
local targets = {
    ["cold-shard"] = {
        function_name = "weft-cold-shard-query",
        image         = "weft-lambda-shard",
        cmd_path      = "./cmd/lambda-shard/",
    },
    ["compactor"] = {
        function_name = "weft-compactor",
        image         = "weft-lambda-compactor",
        cmd_path      = "./cmd/lambda-compactor/",
    },
}

function run(args)
    local name = (args and args[1]) or "cold-shard"
    local t = targets[name]
    if not t then
        error("unknown lambda target: " .. name .. " (want cold-shard | compactor)")
    end
    local zip_path = "/tmp/" .. t.image .. ".zip"

    -- CGO cross-compilation from macOS to Linux requires Docker
    -- (Linux-specific syscalls like clearenv/setresgid don't exist on macOS)
    kevcons.log("building " .. name .. " via Docker (x86_64)...")
    kevcons.exec("docker", {"build", "-f", "Dockerfile.lambda",
        "--build-arg", "CMD_PATH=" .. t.cmd_path, "-t", t.image, ".."})

    -- Extract binary from image
    local container = kevcons.exec("docker", {"create", t.image})
    container = container:match("^%s*(.-)%s*$")
    kevcons.exec("docker", {"cp", container .. ":/var/runtime/bootstrap", "/tmp/bootstrap"})
    kevcons.exec("docker", {"rm", container})

    -- Guard against the arch mismatch that shipped an unrunnable bootstrap
    -- (amd64 ELF on an arm64 Lambda → exec format error → exit 126). The
    -- Lambda is configured x86_64; fail loudly here rather than in prod.
    local arch = kevcons.exec("file", {"/tmp/bootstrap"})
    if not arch:match("x86%-64") then
        os.remove("/tmp/bootstrap")
        error("bootstrap arch mismatch: " .. t.function_name .. " is x86_64 but built: " .. arch)
    end

    -- Package zip
    kevcons.log("packaging zip...")
    kevcons.exec("zip", {"-j", zip_path, "/tmp/bootstrap"})
    os.remove("/tmp/bootstrap")

    -- Deploy to Lambda via cross-account role
    kevcons.log("deploying to " .. t.function_name .. "...")
    kevcons.aws_exec("lambda", "update-function-code",
        "--function-name", t.function_name,
        "--zip-file", "fileb://" .. zip_path)

    kevcons.log("waiting for function update...")
    kevcons.aws_exec("lambda", "wait", "function-updated",
        "--function-name", t.function_name)

    os.remove(zip_path)
    kevcons.log("")
    kevcons.log("deployed " .. t.function_name)
end
