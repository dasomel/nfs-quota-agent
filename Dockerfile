# Build stage
# Pinned to the Go 1.26 toolchain to match go.mod's `go 1.26.0` and the
# go-version pins in .github/workflows/ci.yaml -- a builder ahead of those
# is toolchain drift, not a harmless bump. Digest is the multi-arch manifest
# list for golang:1.26-alpine (1.26.8-alpine3.24), resolved via
# `docker buildx imagetools inspect golang:1.26-alpine` on 2026-09-02, so it
# covers both amd64 and arm64 rather than pinning a single platform.
FROM golang:1.26-alpine@sha256:b6890e35ded5d19118c2bca3d7754dc4e6f694aac2d0aeb92f9807c2879e4230 AS builder

# Pinned to the full closure of packages this stage adds: git plus every
# transitive dependency apk pulled in for it, computed as (packages in this
# stage's image) minus (packages in the bare digest-pinned golang:1.26-alpine
# base above) via `apk info -v`, resolved on 2026-09-02. The base digest
# already freezes everything else (the base image's own packages); this pins
# what apk would otherwise resolve fresh from the mutable Alpine 3.24 index.
# A pin that vanishes from that index fails the build loudly rather than
# drifting silently; bumping the base digest is the moment to refresh these.
RUN apk add --no-cache \
      brotli-libs=1.2.0-r1 \
      c-ares=1.34.8-r0 \
      git-init-template=2.54.0-r0 \
      git=2.54.0-r0 \
      libcurl=8.21.0-r0 \
      libexpat=2.8.4-r0 \
      libidn2=2.3.8-r0 \
      libpsl=0.21.5-r3 \
      libunistring=1.4.2-r0 \
      nghttp2-libs=1.69.0-r0 \
      pcre2=10.48-r0 \
      zstd-libs=1.5.7-r2

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
# Pinned to the full closure of packages this stage adds: xfsprogs-extra,
# quota-tools, e2fsprogs, util-linux, btrfs-progs, and every transitive
# dependency apk pulled in for them, computed as (packages in this image)
# minus (packages in the bare digest-pinned alpine:3.24 base above) via
# `apk info -v`, resolved on 2026-09-02. The base digest already freezes
# everything else (the base image's own packages); this pins what apk would
# otherwise resolve fresh from the mutable Alpine 3.24 index. A pin that
# vanishes from that index fails the build loudly rather than drifting
# silently; bumping the base digest is the moment to refresh these.
RUN apk add --no-cache \
      agetty=2.42.1-r0 \
      blkid=2.42.1-r0 \
      btrfs-progs=6.17.1-r1 \
      cfdisk=2.42.1-r0 \
      device-mapper-libs=2.03.35-r3 \
      dmesg=2.42.1-r0 \
      e2fsprogs-libs=1.47.4-r0 \
      e2fsprogs=1.47.4-r0 \
      eudev-libs=3.2.14-r6 \
      findmnt=2.42.1-r0 \
      flock=2.42.1-r0 \
      fstrim=2.42.1-r0 \
      gdbm=1.26-r0 \
      hexdump=2.42.1-r0 \
      inih=62-r0 \
      libblkid=2.42.1-r0 \
      libbz2=1.0.8-r6 \
      libcap-ng=0.8.5-r2 \
      libcom_err=1.47.4-r0 \
      libeconf=0.8.3-r0 \
      libedit=20260508.3.1-r1 \
      libexpat=2.8.4-r0 \
      libfdisk=2.42.1-r0 \
      libffi=3.5.2-r1 \
      libgcc=15.2.0-r5 \
      libmount=2.42.1-r0 \
      libncursesw=6.6_p20260516-r0 \
      libpanelw=6.6_p20260516-r0 \
      libsmartcols=2.42.1-r0 \
      libstdc++=15.2.0-r5 \
      libuuid=2.42.1-r0 \
      linux-pam=1.7.1-r2 \
      logger=2.42.1-r0 \
      losetup=2.42.1-r0 \
      lsblk=2.42.1-r0 \
      lscpu=2.42.1-r0 \
      lzo=2.10-r5 \
      mcookie=2.42.1-r0 \
      mount=2.42.1-r0 \
      mpdecimal=4.0.1-r0 \
      ncurses-terminfo-base=6.6_p20260516-r0 \
      partx=2.42.1-r0 \
      pyc=3.14.7-r1 \
      python3-pyc=3.14.7-r1 \
      python3-pycache-pyc0=3.14.7-r1 \
      python3=3.14.7-r1 \
      quota-tools=4.11-r0 \
      readline=8.3.3-r1 \
      runuser=2.42.1-r0 \
      setarch=2.42.1-r0 \
      setpriv=2.42.1-r0 \
      sfdisk=2.42.1-r0 \
      skalibs-libs=2.15.0.0-r0 \
      sqlite-libs=3.53.4-r0 \
      umount=2.42.1-r0 \
      userspace-rcu=0.15.3-r0 \
      util-linux-misc=2.42.1-r0 \
      util-linux=2.42.1-r0 \
      utmps-libs=0.1.3.3-r0 \
      uuidgen=2.42.1-r0 \
      wipefs=2.42.1-r0 \
      xfsprogs-extra=7.0.1-r0 \
      xfsprogs=7.0.1-r0 \
      xz-libs=5.8.3-r0 \
      zstd-libs=1.5.7-r2 && \
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
