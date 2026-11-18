# syntax=docker/dockerfile:1
#
# docker build --build-arg VERSION=0.1.0 --build-arg REVISION=$(git rev-parse HEAD) -t watchfor-prometheus-exporter .
#
# The binary is cross-compiled on the build host (no emulation), static
# (CGO_ENABLED=0) and built with the same flags as the release archives.
# Base images are pinned by digest; Dependabot proposes the bumps. The Go
# image must match the toolchain line in go.mod (GOTOOLCHAIN=local).

FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.Revision=${REVISION}" \
      -o /out/watchfor-prometheus-exporter ./cmd/watchfor-prometheus-exporter

# distroless/static: no shell, no package manager, runs as uid 65532.
# Nothing is written to disk, so the container runs with a read-only root
# filesystem.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG VERSION=dev
LABEL org.opencontainers.image.title="watchfor-prometheus-exporter" \
      org.opencontainers.image.description="Prometheus exporter for WatchFor uptime monitoring" \
      org.opencontainers.image.source="https://github.com/watchfor-io/prometheus-exporter" \
      org.opencontainers.image.url="https://watchfor.io" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="WatchFor" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/watchfor-prometheus-exporter /usr/local/bin/watchfor-prometheus-exporter
USER 65532:65532
EXPOSE 10056
ENTRYPOINT ["/usr/local/bin/watchfor-prometheus-exporter"]
