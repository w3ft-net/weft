# weft-demo-service: bundles weftd + a Python rate-shape log generator.
#
# Build context must be ~/dev (the parent of weft/ and warp/) so the
# weftd build can find its sibling warp checkout — same convention as
# the main Dockerfile.
#
#   docker build -f weft/dev/demo/Dockerfile -t weft-demo-service ~/dev

FROM golang:1.26 AS weftd-builder
WORKDIR /src
COPY warp /src/warp
COPY saaskit /src/saaskit
COPY weft /src/weft
WORKDIR /src/weft
RUN rm -f go.work go.work.sum \
 && go mod edit -replace github.com/graybeardcorp/warp=/src/warp \
                -replace github.com/graybeardcorp/saaskit=/src/saaskit
RUN go mod download
RUN CGO_ENABLED=0 go build -o /out/weftd ./cmd/weftd

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=weftd-builder /out/weftd /usr/local/bin/weftd
COPY weft/dev/demo/generator.py /usr/local/bin/generator.py
COPY weft/dev/demo/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/weftd /usr/local/bin/generator.py /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
