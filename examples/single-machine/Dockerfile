# The single-machine weft image: every role (api, coordinator, compute, shard) in
# one image, selected by ROLE (deploy/entrypoint.sh). The build context is the
# directory that holds weft-core, so the private warp module can be copied in
# beside it:
#
#   docker compose build        (from this directory; the compose file sets the context)

FROM golang:1.26 AS builder

WORKDIR /src
COPY warp /src/warp
COPY warp-lua /src/warp-lua
COPY weft-core /src/weft-core
WORKDIR /src/weft-core

# warp and warp-lua are resolved from the copied siblings, so the build needs no module-fetch
# credentials. The workspace (go.work) ties core and this example together. The
# default shape is generated here so the image never takes whatever overlay the
# working tree carries.
RUN go work edit -replace github.com/graybeardcorp/warp=/src/warp -replace github.com/graybeardcorp/warp-lua=/src/warp-lua \
 && go mod download \
 && go run ./cmd/shapegen -spec shape/specs/default.yaml -out shape/coord_gen.go

WORKDIR /src/weft-core/examples/single-machine
RUN CGO_ENABLED=1 go build -o /bin/weft-shard ./cmd/shard
RUN CGO_ENABLED=1 go build -o /bin/weft-compute ./cmd/compute
RUN CGO_ENABLED=1 go build -o /bin/weft-coordinator ./cmd/coordinator
RUN CGO_ENABLED=0 go build -o /bin/weft-api ./cmd/api
WORKDIR /src/weft-core
RUN CGO_ENABLED=0 go build -o /bin/weftd ./cmd/weftd

# Same distro as the builder, to match glibc for the cgo binaries. docker-cli is
# for the coordinator role: the Docker launcher runs sibling shard containers
# through the bind-mounted /var/run/docker.sock.
FROM golang:1.26
RUN apt-get update \
 && apt-get install -y ca-certificates docker-cli git \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/weft-shard /bin/weft-compute /bin/weft-coordinator /bin/weft-api /bin/weftd /usr/local/bin/
COPY --from=builder /src/weft-core/examples/single-machine/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
