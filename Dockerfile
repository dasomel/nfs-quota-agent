# Build stage
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder

RUN apk add --no-cache git

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o /nfs-quota-agent ./cmd/nfs-quota-agent

# Runtime stage
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d

LABEL maintainer="dasomell@gmail.com" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/dasomel/nfs-quota-agent"

# Install filesystem quota tools:
# - xfsprogs-extra: for xfs_quota command (XFS support)
# - quota-tools: for setquota command (ext4 support)
# - e2fsprogs: for chattr command (ext4 project attribute)
# - util-linux: for findmnt command (mount options check)
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
RUN apk add --no-cache xfsprogs-extra quota-tools e2fsprogs util-linux && \
    mkdir -p /licenses && \
    apk info -a xfsprogs-extra quota-tools e2fsprogs util-linux 2>/dev/null \
      | grep -A1 -i 'license:' > /licenses/os-packages-manifest.txt || true && \
    if [ ! -s /licenses/os-packages-manifest.txt ]; then \
      echo "ERROR: could not record OS package licenses; apk metadata format may have changed" >&2; \
      exit 1; \
    fi

COPY --from=builder /nfs-quota-agent /nfs-quota-agent
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md /licenses/
COPY licenses/gnu/ /licenses/gnu/

ENTRYPOINT ["/nfs-quota-agent"]
