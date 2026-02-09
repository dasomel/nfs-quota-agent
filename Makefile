# Copyright 2024 dasomel
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

BINARY_NAME=nfs-quota-agent
REGISTRY?=ghcr.io/dasomel
IMAGE_NAME=$(REGISTRY)/$(BINARY_NAME)
VERSION?=latest
PLATFORMS?=linux/amd64,linux/arm64,linux/arm/v7

.PHONY: all build build-linux clean test test-coverage fmt vet tidy lint \
	docker-build docker-push docker-buildx \
	helm-lint helm-package helm-install helm-uninstall

all: build

# Build binary for current platform
build:
	@echo "Building $(BINARY_NAME)..."
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/$(BINARY_NAME) ./cmd/$(BINARY_NAME)

# Build binaries for all Linux platforms
build-linux:
	@echo "Building $(BINARY_NAME) for Linux..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/$(BINARY_NAME)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/$(BINARY_NAME)-linux-arm64 ./cmd/$(BINARY_NAME)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o bin/$(BINARY_NAME)-linux-armv7 ./cmd/$(BINARY_NAME)

clean:
	rm -rf bin/
	rm -rf .helm-releases/

# Run tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Format code
fmt:
	go fmt ./...

# Run go vet
vet:
	go vet ./...

# Tidy dependencies
tidy:
	go mod tidy

# Run golangci-lint (requires golangci-lint installed)
lint:
	golangci-lint run

# Build Docker image
docker-build:
	docker build -t $(IMAGE_NAME):$(VERSION) .

# Push Docker image
docker-push: docker-build
	docker push $(IMAGE_NAME):$(VERSION)

# Build and push multi-arch image using buildx
docker-buildx:
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE_NAME):$(VERSION) --push .

# Build multi-arch image locally (no push)
docker-buildx-local:
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE_NAME):$(VERSION) --load .

# Lint Helm chart
helm-lint:
	helm lint ./charts/$(BINARY_NAME)

# Package Helm chart
helm-package:
	@mkdir -p .helm-releases
	helm package ./charts/$(BINARY_NAME) -d .helm-releases

# Install using Helm
helm-install:
	helm install $(BINARY_NAME) ./charts/$(BINARY_NAME) \
		--namespace $(BINARY_NAME) \
		--create-namespace

# Uninstall Helm release
helm-uninstall:
	helm uninstall $(BINARY_NAME) -n $(BINARY_NAME)

# Show help
help:
	@echo "Available targets:"
	@echo "  build            - Build binary for current platform"
	@echo "  build-linux      - Build binaries for Linux (amd64, arm64, armv7)"
	@echo "  clean            - Remove build artifacts"
	@echo "  test             - Run tests"
	@echo "  test-coverage    - Run tests with coverage report"
	@echo "  fmt              - Format code"
	@echo "  vet              - Run go vet"
	@echo "  tidy             - Tidy go modules"
	@echo "  lint             - Run golangci-lint"
	@echo "  docker-build     - Build Docker image"
	@echo "  docker-push      - Build and push Docker image"
	@echo "  docker-buildx    - Build and push multi-arch image"
	@echo "  helm-lint        - Lint Helm chart"
	@echo "  helm-package     - Package Helm chart"
	@echo "  helm-install     - Install using Helm"
	@echo "  helm-uninstall   - Uninstall Helm release"
