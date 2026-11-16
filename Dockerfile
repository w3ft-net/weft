# The open-source weft image: every role (api, coordinator, compute, shard) in
# one image, selected by ROLE (deploy/entrypoint.sh). Build context is the
# parent directory, so the private warp module can be copied in beside core:
#
#   docker compose build        (from weft-core; the compose file sets context: ..)

FROM golang:1.26 AS builder

WORKDIR /src
COPY warp /src/warp
COPY weft-core /src/weft-core
WORKDIR /src/weft-core

# warp is resolved from the copied sibling, so the build needs no module-fetch
# credentials. The default shape is generated here so the image never takes
# whatever overlay the working tree carries.
RUN rm -f go.work go.work.sum \
 && go mod edit -replace github.com/graybeardcorp/warp=/src/warp \
 && go mod download \
 && go run ./cmd/shapegen -spec shape/specs/default.yaml -out shape/coord_gen.go

RUN CGO_ENABLED=1 go build -o /bin/weft-shard ./cmd/shard
# localreader links DuckDB so compute reads archives itself, with no Lambda. A
# build without it is cgo-free and reads them only through a Lambda.
RUN CGO_ENABLED=1 go build -tags localreader -o /bin/weft-compute ./cmd/compute
RUN CGO_ENABLED=1 go build -o /bin/weft-coordinator ./cmd/coordinator
RUN CGO_ENABLED=0 go build -o /bin/weft-api ./cmd/api
RUN CGO_ENABLED=0 go build -o /bin/weftd ./cmd/weftd

# Same distro as the builder, to match glibc for the cgo binaries. docker-cli is
# for the coordinator role: the Docker launcher runs sibling shard containers
# through the bind-mounted /var/run/docker.sock.
FROM golang:1.26
RUN apt-get update \
 && apt-get install -y ca-certificates docker-cli git \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/weft-shard /bin/weft-compute /bin/weft-coordinator /bin/weft-api /bin/weftd /usr/local/bin/
COPY --from=builder /src/weft-core/deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
