# Build context must be the parent directory (~/dev) so we can access both weft and warp.
# docker compose -f weft/docker-compose.yml up --build

FROM golang:1.26 AS builder

WORKDIR /src

# Copy warp first (local dependency).
COPY warp /src/warp

# Copy weft.
COPY weft /src/weft
WORKDIR /src/weft

# Fix the replace directive for the container layout.
RUN sed -i 's|=> ../warp|=> /src/warp|' go.mod

RUN go mod download
RUN CGO_ENABLED=1 go build -o /bin/weft-shard ./cmd/shard
RUN CGO_ENABLED=1 go build -o /bin/weft-compute ./cmd/compute
RUN CGO_ENABLED=1 go build -o /bin/weft-coordinator ./cmd/coordinator
RUN CGO_ENABLED=1 go build -o /bin/weft-api ./cmd/api
RUN CGO_ENABLED=1 go build -o /bin/lambda-shard ./cmd/lambda-shard
RUN CGO_ENABLED=0 go build -o /bin/weftd ./cmd/weftd

# Use the same distro as the builder to match glibc.
FROM golang:1.26
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/weft-shard /bin/weft-compute /bin/weft-coordinator /bin/weft-api /bin/lambda-shard /bin/weftd /usr/local/bin/
COPY --from=builder /src/weft/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
