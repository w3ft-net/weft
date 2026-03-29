# Build context must be the parent directory (~/dev) so we can access both weft and warp.
# docker build -f weft/Dockerfile -t weft ~/dev
# Or use docker-compose which sets context: ..

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

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/weft-shard /bin/weft-compute /usr/local/bin/
