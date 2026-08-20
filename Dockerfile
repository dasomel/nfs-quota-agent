# Build stage
FROM golang:1.26-alpine AS builder

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
FROM alpine:3.21

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
# shipped in this image — apk pulls whatever alpine:3.21 currently carries,
# which drifts with upstream security patches even though the tag is pinned.
RUN apk add --no-cache xfsprogs-extra quota-tools e2fsprogs util-linux && \
    mkdir -p /licenses && \
    apk info -a xfsprogs-extra quota-tools e2fsprogs util-linux 2>/dev/null \
      | grep -A1 'license:' > /licenses/os-packages-manifest.txt

COPY --from=builder /nfs-quota-agent /nfs-quota-agent
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md /licenses/
COPY licenses/gnu/ /licenses/gnu/

ENTRYPOINT ["/nfs-quota-agent"]
