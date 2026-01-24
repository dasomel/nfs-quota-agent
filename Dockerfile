# Build stage
FROM golang:1.22-alpine AS builder

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

LABEL maintainer="dasomell@gmail.com"

# Install filesystem quota tools:
# - xfsprogs: for xfs_quota command (XFS support)
# - quota-tools: for setquota command (ext4 support)
# - e2fsprogs: for chattr command (ext4 project attribute)
RUN apk add --no-cache xfsprogs quota-tools e2fsprogs

COPY --from=builder /nfs-quota-agent /nfs-quota-agent

ENTRYPOINT ["/nfs-quota-agent"]
