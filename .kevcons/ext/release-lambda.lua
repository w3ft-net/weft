description = "Build and deploy the Lambda cold shard binary (usage: kevcons ext release-lambda)"

local function_name = "weft-cold-shard-query"
local zip_path = "/tmp/weft-lambda-shard.zip"

function run(args)
    -- CGO cross-compilation from macOS to Linux requires Docker
    -- (Linux-specific syscalls like clearenv/setresgid don't exist on macOS)

    -- Build via Docker
    kevcons.log("building lambda-shard via Docker (x86_64)...")
    kevcons.exec("docker", {"build", "-f", "Dockerfile.lambda", "-t", "weft-lambda-shard", ".."})

    -- Extract binary from image
    local container = kevcons.exec("docker", {"create", "weft-lambda-shard"})
    container = container:match("^%s*(.-)%s*$")
    kevcons.exec("docker", {"cp", container .. ":/var/runtime/bootstrap", "/tmp/bootstrap"})
    kevcons.exec("docker", {"rm", container})

    -- Guard against the arch mismatch that shipped an unrunnable bootstrap
    -- (amd64 ELF on an arm64 Lambda → exec format error → exit 126). The
    -- Lambda is configured x86_64; fail loudly here rather than in prod.
    local arch = kevcons.exec("file", {"/tmp/bootstrap"})
    if not arch:match("x86%-64") then
        os.remove("/tmp/bootstrap")
        error("bootstrap arch mismatch: weft-cold-shard-query is x86_64 but built: " .. arch)
    end

    -- Package zip
    kevcons.log("packaging zip...")
    kevcons.exec("zip", {"-j", zip_path, "/tmp/bootstrap"})
    os.remove("/tmp/bootstrap")

    -- Deploy to Lambda via cross-account role
    kevcons.log("deploying to " .. function_name .. "...")
    kevcons.aws_exec("lambda", "update-function-code",
        "--function-name", function_name,
        "--zip-file", "fileb://" .. zip_path)

    kevcons.log("waiting for function update...")
    kevcons.aws_exec("lambda", "wait", "function-updated",
        "--function-name", function_name)

    os.remove(zip_path)
    kevcons.log("")
    kevcons.log("deployed " .. function_name)
end
