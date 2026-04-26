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

# BUILD_FLAGS is empty in prod builds (empty string is a no-op to
# `go build`). Integration runs that want per-container coverage
# pass BUILD_FLAGS="-cover -coverpkg=github.com/kevlarcon/weft/..."
# so every server binary writes its own counter file to GOCOVERDIR
# on clean shutdown. See docker-compose.cover.yml.
ARG BUILD_FLAGS=""

RUN CGO_ENABLED=1 go build $BUILD_FLAGS -o /bin/weft-shard ./cmd/shard
RUN CGO_ENABLED=1 go build $BUILD_FLAGS -o /bin/weft-compute ./cmd/compute
RUN CGO_ENABLED=1 go build $BUILD_FLAGS -o /bin/weft-coordinator ./cmd/coordinator
RUN CGO_ENABLED=1 go build $BUILD_FLAGS -o /bin/weft-api ./cmd/api
RUN CGO_ENABLED=1 go build $BUILD_FLAGS -o /bin/lambda-shard ./cmd/lambda-shard
RUN CGO_ENABLED=0 go build $BUILD_FLAGS -o /bin/weftd ./cmd/weftd

# Use the same distro as the builder to match glibc.
FROM golang:1.26
# docker-cli is needed in the coordinator role: DockerLauncher (dev
# mode) execs `docker run` over the bind-mounted /var/run/docker.sock
# to spawn sibling shard containers. The cli alone is small (~50 MB);
# we don't pull in dockerd. Other roles ignore it. ca-certificates is
# still needed for the gRPC tls path on prod.
RUN apt-get update \
 && apt-get install -y ca-certificates docker-cli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/weft-shard /bin/weft-compute /bin/weft-coordinator /bin/weft-api /bin/lambda-shard /bin/weftd /usr/local/bin/
COPY --from=builder /src/weft/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
