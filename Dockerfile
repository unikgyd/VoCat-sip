# syntax=docker/dockerfile:1.7

# Build toolchains run natively on the BuildKit host. Without BUILDPLATFORM,
# the arm64 branch executes npm and the Go compiler through QEMU, which is much
# slower and makes npm ci appear to hang despite producing no progress output.
# ---- Stage 1: build the web frontend once on the native builder ----
FROM --platform=$BUILDPLATFORM node:20-alpine AS web-builder
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- Stage 2: cross-compile the Go binary on the native builder ----
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS go-builder
RUN apt-get update && apt-get install -y --no-install-recommends git && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src

ARG VERSION=0.1.0-dev
ARG BUILD_TIME=""
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# Overlay the freshly built frontend so go:embed web/dist picks it up.
COPY --from=web-builder /web/dist ./web/dist

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOARCH=${TARGETARCH} VERSION="${VERSION}" BUILD_TIME="${BUILD_TIME}" \
    sh scripts/build-linux.sh /out/vocat native

# ---- Stage 3: minimal runtime ----
FROM debian:bookworm-slim AS runtime
# The normal Linux build uses purego's glibc loader. Keep musl/static builds
# separate instead of shipping a binary that cannot start in Alpine.
RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates iproute2 libasound2 libccid libopencore-amrnb0 libpcsclite1 \
      libqmi-utils passwd pcscd tzdata && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --gid 1000 vocat && \
    useradd --uid 1000 --gid vocat --no-create-home vocat

RUN mkdir -p /opt/vocat/bin /opt/vocat/data && \
    chown -R vocat:vocat /opt/vocat

COPY --from=go-builder /out/vocat /opt/vocat/bin/vocat
COPY LICENSE NOTICE /opt/vocat/share/licenses/
COPY third_party/licenses/ /opt/vocat/share/licenses/third-party/
COPY LICENSES/ /opt/vocat/share/licenses/local-dependencies/
COPY scripts/docker-entrypoint.sh /usr/local/bin/vocat-entrypoint

# Symlink into /usr/local/bin so `docker exec <ctr> vocat ...` finds it via $PATH.
RUN ln -s /opt/vocat/bin/vocat /usr/local/bin/vocat && \
    chmod 0755 /usr/local/bin/vocat-entrypoint && \
    /opt/vocat/bin/vocat media-check --require-amr --json

# Hardware access and the bundled pcscd daemon require root inside the
# container. The container already needs host networking and privileged device
# access for modem, QMI, IPsec, and hot-plug support.
USER root
VOLUME ["/opt/vocat/data"]
EXPOSE 7575
ENV VOCAT_ADDR=0.0.0.0:7575 \
    VOCAT_DATABASE_PATH=/opt/vocat/data/vocat.db

ENTRYPOINT ["/usr/local/bin/vocat-entrypoint"]
