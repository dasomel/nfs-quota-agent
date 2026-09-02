# Build stage
# Pinned to the Go 1.26 toolchain to match go.mod's `go 1.26.0` and the
# go-version pins in .github/workflows/ci.yaml -- a builder ahead of those
# is toolchain drift, not a harmless bump. Digest is the multi-arch manifest
# list for golang:1.26-alpine (1.26.8-alpine3.24), resolved via
# `docker buildx imagetools inspect golang:1.26-alpine` on 2026-09-02, so it
# covers both amd64 and arm64 rather than pinning a single platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder

# apk packages are not version-pinned (see the runtime stage's apk add
# comment below for why -- #26 tried pinning the full closure and reverted
# it: Alpine stable removes a superseded package version from its index as
# soon as the next one ships, so a version pin here breaks on essentially
# every upstream bugfix release rather than only on a real regression).
RUN apk add --no-cache git

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Cross-compile natively instead of emulating the Go toolchain (#26):
# --platform=$BUILDPLATFORM above pins this stage to the build host's
# native architecture regardless of the image's target platform, so `go
# build` itself always runs natively -- go mod download and the compiler
# are never emulated under QEMU, only the final binary's target changes.
# Buildx sets TARGETOS/TARGETARCH/TARGETVARIANT per platform automatically;
# GOARM is only meaningful for TARGETARCH=arm, so it is left empty (Go
# ignores an empty GOARM) unless TARGETVARIANT is set, in which case it's
# TARGETVARIANT with the leading "v" stripped (e.g. "v7" -> "7").
ARG TARGETOS TARGETARCH TARGETVARIANT

# Build
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=$(echo "$TARGETVARIANT" | sed 's/^v//') \
      go build -ldflags '-extldflags "-static"' -o /nfs-quota-agent ./cmd/nfs-quota-agent

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
# quota.CommandRunner), never linked into the Go binary.
#
# Reproducibility boundary (#26): the base image digest above and go.sum
# are frozen inputs -- the same digest and the same go.sum always produce
# the same bytes. apk packages are NOT frozen the same way: they resolve
# from the live Alpine 3.24 index at build time and are only RECORDED
# below, not pinned. Pinning every package in the closure
# (name=version-release) was tried and reverted: Alpine stable keeps only
# the current version of each package in its index, so `pcre2=10.47-r1`
# broke within hours of being pinned when the index moved to
# `pcre2=10.48-r0` -- "fail loudly on drift" became "fail on every
# upstream bugfix release", including ones this image never asked for.
# The Image Build job in ci.yaml still catches a package disappearing
# from the index entirely (the build fails), just not a version changing
# under it. dependabot.yml's docker ecosystem is what keeps the base
# digest itself from going stale.
#
# The primary provenance record for exactly which OS package versions
# shipped in a given image is the Syft-based SBOM release.yaml's
# build-and-push job generates for the pushed image (`sbom: true` on the
# docker/build-push-action step), attached to the image as an OCI
# attestation. /licenses/os-packages-manifest.txt below is the build-time
# copy of that same information -- every installed package name and
# version, not just the five requested tools -- kept in the image itself
# for offline inspection and to satisfy the GPL/LGPL written-offer
# requirement for xfsprogs-extra, quota-tools, e2fsprogs, util-linux and
# btrfs-progs without depending on the SBOM being fetched separately. An
# empty manifest would mean shipping GPL binaries with no record of which
# source corresponds to them, so verify it came out non-empty and fail
# the build loudly rather than silently producing an empty file.
RUN apk add --no-cache xfsprogs-extra quota-tools e2fsprogs util-linux btrfs-progs && \
    mkdir -p /licenses && \
    apk info -v | sort > /licenses/os-packages-manifest.txt && \
    if [ ! -s /licenses/os-packages-manifest.txt ]; then \
      echo "ERROR: could not record installed OS packages; apk metadata format may have changed" >&2; \
      exit 1; \
    fi

COPY --from=builder /nfs-quota-agent /nfs-quota-agent
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md /licenses/
COPY licenses/gnu/ /licenses/gnu/

ENTRYPOINT ["/nfs-quota-agent"]
