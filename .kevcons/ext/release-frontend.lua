description = "Build and deploy the weft frontend SPA to S3/CloudFront (usage: kevcons ext release-frontend)"

local bucket = "weft-frontend-220664822695"
local distribution_id_ssm = "/weft/config/cloudfront-distribution-id"

function run(args)
    -- Build the production frontend
    kevcons.log("building frontend...")
    kevcons.exec("bash", {"-c", "cd frontend && VITE_API_URL=https://api.w3ft.net npm run build"})

    -- Sync to S3
    kevcons.log("deploying to s3://" .. bucket .. "/...")
    kevcons.aws_exec("s3", "sync", "frontend/dist/", "s3://" .. bucket .. "/",
        "--delete",
        "--cache-control", "public,max-age=3600")

    -- Set long cache on hashed assets
    kevcons.aws_exec("s3", "cp",
        "s3://" .. bucket .. "/assets/", "s3://" .. bucket .. "/assets/",
        "--recursive",
        "--cache-control", "public,max-age=31536000,immutable",
        "--metadata-directive", "REPLACE")

    -- Invalidate CloudFront cache
    kevcons.log("invalidating CloudFront cache...")
    local dist_id = kevcons.aws_exec("ssm", "get-parameter",
        "--name", distribution_id_ssm,
        "--query", "Parameter.Value",
        "--output", "text")
    dist_id = dist_id:match("^%s*(.-)%s*$")

    if dist_id ~= "" and dist_id ~= "None" then
        kevcons.aws_exec("cloudfront", "create-invalidation",
            "--distribution-id", dist_id,
            "--paths", "/*")
        kevcons.log("invalidation created for " .. dist_id)
    else
        kevcons.log("warning: no CloudFront distribution ID found in SSM")
    end

    kevcons.log("frontend deployed to https://w3ft.net")
end
