# Build stage
# Pinned to the Go 1.26 toolchain to match go.mod's `go 1.26.0` and the
# go-version pins in .github/workflows/ci.yaml -- a builder ahead of those
# is toolchain drift, not a harmless bump. Digest is the multi-arch manifest
# list for golang:1.26-alpine (1.26.8-alpine3.24), resolved via
# `docker buildx imagetools inspect golang:1.26-alpine` on 2026-09-02, so it
# covers both amd64 and arm64 rather than pinning a single platform.
FROM golang:1.26-alpine@sha256:b6890e35ded5d19118c2bca3d7754dc4e6f694aac2d0aeb92f9807c2879e4230 AS builder

# Pinned to what the digest-pinned base above resolved from the Alpine 3.24
# index on 2026-09-02. A pin that vanishes from that index fails the build
# loudly rather than drifting silently; bumping the base digest is the
# moment to refresh it.
RUN apk add --no-cache git=2.54.0-r0

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o /nfs-quota-agent ./cmd/nfs-quota-agent

# Runtime stage
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

LABEL maintainer="dasomell@gmail.com" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/dasomel/nfs-quota-agent"

# Install filesystem quota tools:
# - xfsprogs-extra: for xfs_quota command (XFS support)
# - quota-tools: for setquota command (ext4 support)
# - e2fsprogs: for chattr command (ext4 project attribute)
# - util-linux: for findmnt command (mount options check)
# - btrfs-progs: for btrfs qgroup/subvolume commands (Btrfs support)
# These are GPL/LGPL-licensed and run as separate exec'd processes (see
# quota.CommandRunner), never linked into the Go binary. Record the exact
# installed name-version and license of each here, at build time, so the
# Corresponding Source pointer in NOTICE always matches what actually
# shipped in this image. The base image is now pinned to a digest (see the
# FROM line above), not a floating tag, so apk resolves against a fixed
# package index rather than one that silently drifts with upstream security
# patches -- dependabot.yml's docker ecosystem is what keeps that digest
# from going stale, proposing a bump (and therefore a fresh manifest here)
# on its own schedule rather than never updating at all.
# An empty manifest would mean shipping GPL binaries with no record of which
# source corresponds to them, so verify it came out non-empty and fail the build
# loudly rather than letting grep's exit status decide, or silencing it with
# `|| true` and quietly producing an empty file.
# Pinned to what the digest-pinned alpine:3.24 base above resolved on
# 2026-09-02 (only the packages named here -- their own dependencies are
# resolved by apk and recorded in the manifest below regardless). A pin
# that vanishes from the Alpine 3.24 index fails the build loudly rather
# than drifting silently; bumping the base digest is the moment to refresh
# these versions.
RUN apk add --no-cache \
      xfsprogs-extra=7.0.1-r0 \
      quota-tools=4.11-r0 \
      e2fsprogs=1.47.4-r0 \
      util-linux=2.42.1-r0 \
      btrfs-progs=6.17.1-r1 && \
    mkdir -p /licenses && \
    apk info -a xfsprogs-extra quota-tools e2fsprogs util-linux btrfs-progs 2>/dev/null \
      | grep -A1 -i 'license:' > /licenses/os-packages-manifest.txt || true && \
    if [ ! -s /licenses/os-packages-manifest.txt ]; then \
      echo "ERROR: could not record OS package licenses; apk metadata format may have changed" >&2; \
      exit 1; \
    fi

COPY --from=builder /nfs-quota-agent /nfs-quota-agent
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md /licenses/
COPY licenses/gnu/ /licenses/gnu/

ENTRYPOINT ["/nfs-quota-agent"]
