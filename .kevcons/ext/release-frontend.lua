description = "Build and deploy the weft frontend SPA to S3/CloudFront (usage: kevcons ext release-frontend)"

local bucket = "weft-frontend-220664822695"
local distribution_id_ssm = "/weft/config/cloudfront-distribution-id"

function run(args)
    -- Build the production frontend
    kevcons.log("building frontend...")
    kevcons.exec("bash", {"-c", "cd frontend && VITE_API_URL=https://api.w3ft.net npm run build"})

    -- Sync to S3 with correct MIME types
    kevcons.log("deploying to s3://" .. bucket .. "/...")

    -- HTML files
    kevcons.aws_exec("s3", "sync", "frontend/dist/", "s3://" .. bucket .. "/",
        "--delete",
        "--exclude", "*",
        "--include", "*.html",
        "--content-type", "text/html",
        "--cache-control", "public,max-age=60")

    -- JS files (hashed, long cache)
    kevcons.aws_exec("s3", "sync", "frontend/dist/", "s3://" .. bucket .. "/",
        "--exclude", "*",
        "--include", "*.js",
        "--content-type", "application/javascript",
        "--cache-control", "public,max-age=31536000,immutable")

    -- CSS files (hashed, long cache)
    kevcons.aws_exec("s3", "sync", "frontend/dist/", "s3://" .. bucket .. "/",
        "--exclude", "*",
        "--include", "*.css",
        "--content-type", "text/css",
        "--cache-control", "public,max-age=31536000,immutable")

    -- SVG files
    kevcons.aws_exec("s3", "sync", "frontend/dist/", "s3://" .. bucket .. "/",
        "--exclude", "*",
        "--include", "*.svg",
        "--content-type", "image/svg+xml",
        "--cache-control", "public,max-age=3600")

    -- Everything else (images, fonts, etc.)
    kevcons.aws_exec("s3", "sync", "frontend/dist/", "s3://" .. bucket .. "/",
        "--exclude", "*.html",
        "--exclude", "*.js",
        "--exclude", "*.css",
        "--exclude", "*.svg",
        "--cache-control", "public,max-age=3600")

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
