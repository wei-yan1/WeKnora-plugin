# Multi-stage build for the TARily Web Search plugin as an isolated OCI plugin.
# The final image is a static Go binary that listens on the Unix socket the host
# mounts at /run/weknora/plugin.sock and makes outbound calls through the
# host-side egress proxy mounted at /run/weknora-egress/egress.sock.
#
# The plugin's go.mod uses `replace github.com/Tencent/WeKnora => ../../../WeKnora-fork`,
# which cannot be resolved inside the image (v0.0.0 is a local-replace placeholder).
# build.sh therefore runs `go mod vendor` on the host first (where the fork
# checkout exists), and this Dockerfile builds strictly from vendored sources.
#
# Build via:
#   ./build.sh

# ---- Stage 1: build -----------------------------------------------------
# Mirror prefix: registry-1.docker.io is unreachable from CN networks
# (DNS-poisoned / reset). daocloud mirrors Docker Hub library images.
FROM docker.m.daocloud.io/library/golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor ./vendor
COPY main.go ./

# Build a static, CGO-free Linux binary from the vendored SDK.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -mod=vendor -buildvcs=false -o /out/plugin .

# ---- Stage 2: runtime ----------------------------------------------------
# Scratch keeps the image minimal. The binary is static, so no libc is needed.
FROM scratch

# Copy the CA bundle so HTTPS to api.tavily.com works out of the box.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/plugin /plugin

# The host mounts:
#   /run/weknora         (rw)  -> plugin listens on /run/weknora/plugin.sock
#   /run/weknora-egress  (ro)  -> egress proxy socket for outbound calls
ENTRYPOINT ["/plugin"]
