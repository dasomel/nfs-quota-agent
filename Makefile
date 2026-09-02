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
RELEASE_DIR?=.

.PHONY: all build build-linux clean test test-coverage fmt vet tidy lint \
	license sbom generate compat-matrix compat-matrix-validate verify-release \
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
	rm -rf sbom/

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

# Regenerate THIRD_PARTY_LICENSES.md from go.mod/go.sum and fail on any dependency license not
# in hack/allowed-licenses.txt. Generate into a temp file first: a plain `> THIRD_PARTY_LICENSES.md`
# truncates the existing inventory before go-licenses runs, so a failed run would leave an empty
# file behind. The explicit allowlist (rather than go-licenses' default forbidden/unknown
# classification) is the machine-readable license policy #16 asks for -- a new dependency under a
# license not already reviewed and added to that file fails the build instead of silently passing
# because it happens not to be in go-licenses' built-in forbidden set.
license:
	go tool go-licenses report ./... --template hack/third-party-licenses.tmpl > THIRD_PARTY_LICENSES.md.tmp
	mv THIRD_PARTY_LICENSES.md.tmp THIRD_PARTY_LICENSES.md
	go tool go-licenses check ./... --allowed_licenses="$$(paste -sd, hack/allowed-licenses.txt)"

# Regenerate deepcopy methods and the CRD manifest for internal/apis/quota/v1alpha1
# from its kubebuilder markers. Must be re-run (and the result committed) whenever
# types.go changes; CI's "Generate Check" job fails the build if this goes stale.
generate:
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./internal/apis/quota/v1alpha1/...
	go tool controller-gen crd paths=./internal/apis/quota/v1alpha1/... output:crd:dir=charts/nfs-quota-agent/crds

# Generate SBOM (SPDX + CycloneDX) for the Go dependency tree via trivy
sbom:
	@command -v trivy >/dev/null 2>&1 || { echo "trivy not found — install it from https://trivy.dev to generate an SBOM"; exit 1; }
	@mkdir -p sbom
	trivy fs --format spdx-json --output sbom/sbom.spdx.json .
	trivy fs --format cyclonedx --output sbom/sbom.cyclonedx.json .
	@echo "SBOM written to sbom/sbom.spdx.json and sbom/sbom.cyclonedx.json"

# Validate hack/compatibility-matrix.json against its JSON Schema
# (hack/compatibility-matrix.schema.json) -- required top-level fields,
# every section's shape, the closed status enum, evidence required
# whenever status is "verified", and no unknown keys anywhere. See
# hack/validate-compatibility-matrix.py for exactly which JSON Schema
# keywords this stdlib-only validator understands.
compat-matrix-validate:
	@python3 hack/validate-compatibility-matrix.py

# Validate hack/compatibility-matrix.json is well-formed and every entry
# carries a status and evidence field -- the machine-readable
# filesystem/architecture/Kubernetes-version support matrix #5 asks for.
# This only checks shape, not truth: keeping "status" honest against what
# has actually been observed is a human judgment call made when editing
# the file, same as hack/allowed-licenses.txt. Depends on
# compat-matrix-validate for the full JSON Schema check (closed status
# enum, additionalProperties, verified-requires-evidence); this target's
# own inline check stays as a lightweight, dependency-free smoke test.
compat-matrix: compat-matrix-validate
	@python3 -c "\
import json, sys; \
data = json.load(open('hack/compatibility-matrix.json')); \
sections = ['filesystemBackends', 'architectures', 'kubernetesVersions']; \
missing = [(s, e) for s in sections for e in data.get(s, []) if 'status' not in e or 'evidence' not in e]; \
sys.exit('missing status/evidence in: ' + repr(missing)) if missing else print('compatibility-matrix.json OK (' + str(sum(len(data.get(s, [])) for s in sections)) + ' entries)')"

# Offline-verify a downloaded release bundle (binaries/chart/sbom/
# compatibility-matrix) against release-manifest.json's recorded digests.
# RELEASE_DIR defaults to the current directory -- point it at wherever
# you downloaded a release's assets. See hack/verify-release.py for what
# it does and does not check (it cannot verify the container image
# without registry access).
verify-release:
	@python3 hack/verify-release.py $(RELEASE_DIR)

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
	@echo "  generate         - Regenerate CRD deepcopy code and manifest (controller-gen)"
	@echo "  license          - Regenerate THIRD_PARTY_LICENSES.md and check for forbidden licenses"
	@echo "  sbom             - Generate SBOM (SPDX + CycloneDX) via trivy"
	@echo "  docker-build     - Build Docker image"
	@echo "  docker-push      - Build and push Docker image"
	@echo "  docker-buildx    - Build and push multi-arch image"
	@echo "  helm-lint        - Lint Helm chart"
	@echo "  helm-package     - Package Helm chart"
	@echo "  helm-install     - Install using Helm"
	@echo "  helm-uninstall   - Uninstall Helm release"
