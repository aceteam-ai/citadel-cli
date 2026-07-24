# Init-container image for the nvr catalog module (aceteam-ai/citadel-cli#597):
# ghcr.io/aceteam-ai/nvr-config. It compiles the `nvrconfig` command
# (cmd/nvrconfig) so the config.yml generation + nas network-fs verification run
# from the SAME tested Go logic as the rest of citadel (internal/nvr) — no parallel
# script to drift.
#
# IMPORTANT: the build context is the REPOSITORY ROOT (not services/nvr-service),
# because the binary depends on the module's go.mod + internal/nvr. The CI
# workflow (.github/workflows/build-nvr-service.yml) sets context: . accordingly.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/nvrconfig ./cmd/nvrconfig

FROM alpine:3.20
RUN adduser -D -u 10001 nvr
COPY --from=build /out/nvrconfig /usr/local/bin/nvrconfig
# Runs as root: the nas write-check and writing into the (possibly root-squashed)
# /config bind mount need it, matching Frigate's own in-container root identity.
ENTRYPOINT ["/usr/local/bin/nvrconfig"]
