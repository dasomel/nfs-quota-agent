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
CHART_FILE?=charts/$(BINARY_NAME)/Chart.yaml

.PHONY: all build build-linux clean test test-coverage fmt vet tidy lint \
	license sbom generate compat-matrix compat-matrix-validate verify-release \
	docker-build docker-push docker-buildx \
	helm-lint helm-rbac-check helm-package helm-install helm-uninstall update-chart-digest \
	release-bundle release-manifest-local release-preflight

# values.yaml to write image.digest into -- see update-chart-digest below.
VALUES_FILE?=charts/$(BINARY_NAME)/values.yaml

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
# in hack/allowed-licenses.txt. scripts/ci/check-third-party-licenses.sh writes into a temp file
# first and retries go-licenses' license-URL discovery, which can fail transiently for
# golang.org/x/* modules (#95) -- a plain `> THIRD_PARTY_LICENSES.md` would truncate the existing
# inventory before go-licenses runs, so a failed run would leave an empty file behind. The explicit
# allowlist (rather than go-licenses' default forbidden/unknown classification) is the
# machine-readable license policy #16 asks for -- a new dependency under a license not already
# reviewed and added to that file fails the build instead of silently passing because it happens
# not to be in go-licenses' built-in forbidden set.
license:
	bash scripts/ci/check-third-party-licenses.sh write
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

# Produce a schemaVersion 4 release-manifest.json locally from the
# working tree (#26), so the schema (in particular the "provenance" fields
# release.yaml's release-binaries/release-manifest jobs add) can be
# exercised with `hack/verify-release.py --source .` without waiting for a
# real tagged release. This is NOT a substitute for the real release
# pipeline: it builds a single host-platform binary (not the three
# cross-compiled release binaries), has no cosign signatures (empty
# "signatures" left out entirely rather than faked), and uses a
# placeholder all-zero image digest since no image is built/pushed here.
# Anything it CAN compute for real -- go.sum/go.mod hashes, the Go
# toolchain version, the source tree hash, the Dockerfile FROM digests, a
# real chart package and binary checksum -- it computes for real rather
# than faking, so a mismatch caught by --source . against this output is a
# genuine local signal, not a fixture artifact.
RELEASE_MANIFEST_LOCAL_DIR?=.release-manifest-local

release-manifest-local:
	@rm -rf "$(RELEASE_MANIFEST_LOCAL_DIR)"
	@mkdir -p "$(RELEASE_MANIFEST_LOCAL_DIR)"
	CGO_ENABLED=0 go build -o "$(RELEASE_MANIFEST_LOCAL_DIR)/nfs-quota-agent" ./cmd/nfs-quota-agent
	@cd "$(RELEASE_MANIFEST_LOCAL_DIR)" && sha256sum nfs-quota-agent > checksums.txt
	@helm package ./charts/nfs-quota-agent -d "$(RELEASE_MANIFEST_LOCAL_DIR)" >/dev/null
	@cp hack/compatibility-matrix.json "$(RELEASE_MANIFEST_LOCAL_DIR)/compatibility-matrix.json"
	@chart_file=$$(basename "$$(ls $(RELEASE_MANIFEST_LOCAL_DIR)/*.tgz | head -1)"); \
	chart_sha256=$$(sha256sum "$(RELEASE_MANIFEST_LOCAL_DIR)/$$chart_file" | cut -d' ' -f1); \
	compat_sha256=$$(sha256sum hack/compatibility-matrix.json | cut -d' ' -f1); \
	go_version=$$(go version | awk '{print $$3}'); \
	go_sum_sha256=$$(sha256sum go.sum | cut -d' ' -f1); \
	go_mod_sha256=$$(sha256sum go.mod | cut -d' ' -f1); \
	source_tree_hash=$$(git rev-parse 'HEAD^{tree}'); \
	commit=$$(git rev-parse HEAD); \
	builder_full=$$(grep -m1 '^FROM' Dockerfile | grep -oE '[^ ]+@sha256:[0-9a-f]{64}'); \
	runtime_full=$$(grep '^FROM' Dockerfile | tail -1 | grep -oE '[^ ]+@sha256:[0-9a-f]{64}'); \
	builder_ref=$${builder_full%@*}; builder_digest=$${builder_full#*@}; \
	runtime_ref=$${runtime_full%@*}; runtime_digest=$${runtime_full#*@}; \
	awk '{print "{\"file\":\""$$2"\",\"sha256\":\""$$1"\"}"}' "$(RELEASE_MANIFEST_LOCAL_DIR)/checksums.txt" | jq -s . > "$(RELEASE_MANIFEST_LOCAL_DIR)/binaries.json"; \
	jq -n \
	  --arg tag "local-$$commit" \
	  --arg commit "$$commit" \
	  --arg workflow_run "local (make release-manifest-local, not a real release run)" \
	  --arg image "local/nfs-quota-agent" \
	  --arg image_digest "sha256:$$(printf '0%.0s' $$(seq 1 64))" \
	  --arg chart_file "$$chart_file" \
	  --arg chart_sha256 "$$chart_sha256" \
	  --arg compat_file "compatibility-matrix.json" \
	  --arg compat_sha256 "$$compat_sha256" \
	  --arg binaryGoVersion "$$go_version" \
	  --arg goSumSha256 "$$go_sum_sha256" \
	  --arg goModSha256 "$$go_mod_sha256" \
	  --arg sourceTreeHash "$$source_tree_hash" \
	  --arg builderRef "$$builder_ref" --arg builderDigest "$$builder_digest" \
	  --arg runtimeRef "$$runtime_ref" --arg runtimeDigest "$$runtime_digest" \
	  --slurpfile binaries "$(RELEASE_MANIFEST_LOCAL_DIR)/binaries.json" \
	  '{ schemaVersion: 4, tag: $$tag, sourceCommit: $$commit, workflowRun: $$workflow_run, image: { repository: $$image, digest: $$image_digest }, chart: { file: $$chart_file, version: "local", sha256: $$chart_sha256 }, binaries: $$binaries[0], compatibilityMatrix: { file: $$compat_file, sha256: $$compat_sha256 }, provenance: { binaryGoVersion: $$binaryGoVersion, goSum: { file: "go.sum", sha256: $$goSumSha256 }, goMod: { file: "go.mod", sha256: $$goModSha256 }, sourceCommit: $$commit, sourceTreeHash: $$sourceTreeHash, builderImage: { repository: $$builderRef, digest: $$builderDigest }, runtimeImage: { repository: $$runtimeRef, digest: $$runtimeDigest } } }' \
	  > "$(RELEASE_MANIFEST_LOCAL_DIR)/release-manifest.json"; \
	jq .provenance "$(RELEASE_MANIFEST_LOCAL_DIR)/release-manifest.json" > "$(RELEASE_MANIFEST_LOCAL_DIR)/provenance.json"; \
	bash scripts/ci/check-provenance-meta.sh "$(RELEASE_MANIFEST_LOCAL_DIR)/provenance.json"
	@cat "$(RELEASE_MANIFEST_LOCAL_DIR)/release-manifest.json"
	@echo "Wrote $(RELEASE_MANIFEST_LOCAL_DIR)/release-manifest.json -- verify with: python3 hack/verify-release.py --source . $(RELEASE_MANIFEST_LOCAL_DIR)"

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

# Fail before publishing a release whose chart metadata does not describe the
# v* tag. CHART_FILE is overrideable so this guard can be exercised on a
# scratch copy without changing the maintainer-controlled release values.
release-preflight:
	@test -n "$(TAG)" || { echo "Set TAG=vX.Y.Z" >&2; exit 1; }
	@test -f "$(CHART_FILE)" || { echo "Chart file not found: $(CHART_FILE)" >&2; exit 1; }
	@tag="$(TAG)"; \
	case "$$tag" in v?*) normalized_tag="$${tag#v}" ;; *) echo "TAG must start with v: $$tag" >&2; exit 1 ;; esac; \
	chart_version=$$(sed -nE 's/^version:[[:space:]]*"?([^"#[:space:]]+)"?[[:space:]]*(#.*)?$$/\1/p' "$(CHART_FILE)"); \
	chart_app_version=$$(sed -nE 's/^appVersion:[[:space:]]*"?([^"#[:space:]]+)"?[[:space:]]*(#.*)?$$/\1/p' "$(CHART_FILE)"); \
	echo "tag=$$normalized_tag"; \
	echo "chart version=$$chart_version"; \
	echo "chart appVersion=$$chart_app_version"; \
	if [ "$$chart_version" != "$$normalized_tag" ] || [ "$$chart_app_version" != "$$normalized_tag" ]; then \
		echo "Release preflight failed: tag, Chart.yaml version, and appVersion must all match" >&2; \
		exit 1; \
	fi

# Build a single offline/air-gap install bundle (#5) from ALREADY-BUILT
# inputs -- this target does not build the image or the chart itself, it
# only packages what IMAGE_REF and CHART_TGZ already point at (build them
# first with e.g. `make docker-buildx-local IMAGE_NAME=... VERSION=...` and
# `make helm-package`, or pass a pushed IMAGE_REF the CI job resolved).
# Contents (see BUNDLE-README.md.tmpl, copied in verbatim as
# BUNDLE-README.md):
#   images/nfs-quota-agent-image.tar   -- OCI archive of IMAGE_REF, exported
#                                          via `skopeo copy --all` (required
#                                          on PATH -- see release-bundle's
#                                          first check; there is
#                                          deliberately no local-rebuild
#                                          fallback, since rebuilding from
#                                          the working tree would silently
#                                          package a DIFFERENT image than
#                                          the one IMAGE_REF names). --all
#                                          matters for a registry pull: it
#                                          preserves the multi-arch
#                                          manifest-list index instead of
#                                          resolving to one platform, which
#                                          is what makes the exported
#                                          index.json's digest equal
#                                          release-manifest.json's
#                                          image.digest -- see
#                                          hack/verify-release.py's
#                                          oci_archive_image_digest()
#                                          docstring for the proof.
#   chart/<name>-<version>.tgz         -- CHART_TGZ, copied in as-is
#   release-manifest.json(.bundle)     -- when RELEASE_DIR (or the CWD)
#                                          already has them, copied in for
#                                          hack/verify-release.py --bundle
#   hack/verify-release.py             -- so the bundle is self-verifying
#                                          without a second checkout
#   hack/sigstore-trusted-root.json    -- pinned Sigstore trust root, so
#                                          `verify-release.py --bundle
#                                          --require-signatures` can verify
#                                          cosign signatures fully offline
#                                          too (default --trusted-root path
#                                          resolves next to the script, so
#                                          this needs no extra flag when run
#                                          from inside the extracted bundle)
#   hack/compatibility-matrix.json
#   BUNDLE-README.md                   -- air-gapped install steps
# Determinism: every file is staged first, then hack/make-deterministic-tarball.py
# (stdlib tarfile, not the platform `tar` binary -- see that script's
# docstring for why: GNU tar's --sort/--mtime/--owner flags aren't
# available/equivalent on macOS's bundled bsdtar) writes the archive with a
# fixed member order and zeroed mtime/uid/gid/uname/gname, so two runs over
# the same inputs produce byte-identical output -- verify with a sha256
# (portable: this target prints one via Python's hashlib rather than
# assuming GNU coreutils' `sha256sum` is installed) on two separate runs.
# SOURCE_DATE_EPOCH defaults to the git commit date so the digest is
# reproducible across machines/clones too, not just across repeated local
# runs; override it explicitly if building outside a git checkout.
BUNDLE_STAGE?=.release-bundle
BUNDLE_VERSION?=$(VERSION)
BUNDLE_FILE?=nfs-quota-agent-$(BUNDLE_VERSION)-offline.tar.gz
IMAGE_REF?=
CHART_TGZ?=
SOURCE_DATE_EPOCH?=$(shell git log -1 --format=%ct 2>/dev/null || date +%s)
# Set to 1 (release.yaml's release-bundle job does) to fail loudly if
# RELEASE_DIR is missing release-manifest.json or its cosign signature
# bundle, instead of silently packaging a bundle that --require-signatures
# can never pass against. Left unset (0) for local/ad hoc bundles built
# before a release-manifest.json exists.
REQUIRE_SIGNED_MANIFEST?=0

release-bundle:
	@test -n "$(IMAGE_REF)" || { echo "Set IMAGE_REF=<repo:tag or repo@digest> (an already-built/pushed image reference) to pin what release-bundle packages"; exit 1; }
	@test -n "$(CHART_TGZ)" || { echo "Set CHART_TGZ=<path to an already-packaged chart .tgz> (e.g. from 'make helm-package')"; exit 1; }
	@test -f "$(CHART_TGZ)" || { echo "CHART_TGZ=$(CHART_TGZ) not found"; exit 1; }
	@rm -rf "$(BUNDLE_STAGE)"
	@mkdir -p "$(BUNDLE_STAGE)/images" "$(BUNDLE_STAGE)/chart" "$(BUNDLE_STAGE)/hack"
	@command -v skopeo >/dev/null 2>&1 || { echo "skopeo is required to export IMAGE_REF as an OCI archive (no fallback: a fallback that rebuilds the image locally would package the current working tree instead of the exact released image -- see Makefile history for why that branch was removed)"; exit 1; }
	@echo "Exporting $(IMAGE_REF) as an OCI archive..."
	@rm -rf "$(BUNDLE_STAGE)/.oci-src"
	@mkdir -p "$(BUNDLE_STAGE)/.oci-src"
	@if docker image inspect "$(IMAGE_REF)" >/dev/null 2>&1; then \
		echo "WARNING: IMAGE_REF is a local docker image -- docker save only ever exports the single platform already loaded in the local daemon, so the resulting OCI archive's index digest will NOT match a real release's multi-arch release-manifest.json image.digest (see hack/verify-release.py's oci_archive_image_digest() docstring). This local-docker-save path is for ad hoc/dev bundles only; the real release-bundle CI job takes the docker:// --all branch below instead." >&2 && \
		docker save "$(IMAGE_REF)" -o "$(BUNDLE_STAGE)/.docker-save.tar" && \
		skopeo copy --all "docker-archive:$(BUNDLE_STAGE)/.docker-save.tar" "oci:$(BUNDLE_STAGE)/.oci-src:latest" && \
		rm -f "$(BUNDLE_STAGE)/.docker-save.tar"; \
	else \
		skopeo copy --all "docker://$(IMAGE_REF)" "oci:$(BUNDLE_STAGE)/.oci-src:latest"; \
	fi
	@python3 hack/make-deterministic-tarball.py "$(BUNDLE_STAGE)/.oci-src" "$(BUNDLE_STAGE)/images/nfs-quota-agent-image.tar" --mtime "$(SOURCE_DATE_EPOCH)"
	@rm -rf "$(BUNDLE_STAGE)/.oci-src"
	@cp "$(CHART_TGZ)" "$(BUNDLE_STAGE)/chart/"
	@cp hack/verify-release.py "$(BUNDLE_STAGE)/hack/verify-release.py"
	@cp hack/sigstore-trusted-root.json "$(BUNDLE_STAGE)/hack/sigstore-trusted-root.json"
	@cp hack/compatibility-matrix.json "$(BUNDLE_STAGE)/hack/compatibility-matrix.json"
	@if [ "$(REQUIRE_SIGNED_MANIFEST)" = "1" ]; then \
		test -f "$(RELEASE_DIR)/release-manifest.json" || { echo "REQUIRE_SIGNED_MANIFEST=1 but $(RELEASE_DIR)/release-manifest.json is missing -- the release-bundle CI job must download it before calling this target, or --require-signatures verification of this bundle will fail with nothing to check against"; exit 1; }; \
		test -f "$(RELEASE_DIR)/release-manifest.json.bundle" || { echo "REQUIRE_SIGNED_MANIFEST=1 but $(RELEASE_DIR)/release-manifest.json.bundle (its cosign signature) is missing -- the release-bundle CI job must download it before calling this target, or --require-signatures verification of the manifest will fail"; exit 1; }; \
	fi
	@if [ -f "$(RELEASE_DIR)/release-manifest.json" ]; then cp "$(RELEASE_DIR)/release-manifest.json" "$(BUNDLE_STAGE)/release-manifest.json"; fi
	@if [ -f "$(RELEASE_DIR)/release-manifest.json.bundle" ]; then cp "$(RELEASE_DIR)/release-manifest.json.bundle" "$(BUNDLE_STAGE)/release-manifest.json.bundle"; fi
	@if [ -f "$(RELEASE_DIR)/sbom.spdx.json" ]; then cp "$(RELEASE_DIR)/sbom.spdx.json" "$(BUNDLE_STAGE)/sbom.spdx.json"; fi
	@sed -e 's|__IMAGE_REF__|$(IMAGE_REF)|g' -e 's|__CHART_TGZ__|$(notdir $(CHART_TGZ))|g' -e 's|__VERSION__|$(BUNDLE_VERSION)|g' \
		hack/BUNDLE-README.md.tmpl > "$(BUNDLE_STAGE)/BUNDLE-README.md"
	@python3 hack/make-deterministic-tarball.py "$(BUNDLE_STAGE)" "$(BUNDLE_FILE)" --mtime "$(SOURCE_DATE_EPOCH)"
	@echo "Wrote $(BUNDLE_FILE)"
	@python3 -c "import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],'rb').read()).hexdigest()+'  '+sys.argv[1])" "$(BUNDLE_FILE)"

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

# StorageClass names are resolved only from PV specs. Rendering must never add
# StorageClass API RBAC, regardless of whether QuotaPolicy is enabled.
helm-rbac-check:
	@test "$$(helm template ./charts/$(BINARY_NAME) | grep -c storageclasses)" -eq 0
	@test "$$(helm template ./charts/$(BINARY_NAME) --set quotaPolicy.enabled=true | grep -c storageclasses)" -eq 0

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

# Pin charts/nfs-quota-agent's image.digest (#5) for an immutable,
# air-gap-safe install. Pass DIGEST=sha256:<64hex> for a digest you
# already have (e.g. from release-manifest.json), or IMAGE=<repo:tag> to
# resolve one from a registry via crane/skopeo/docker buildx (whichever is
# on PATH -- see hack/update-chart-digest.py). VALUES_FILE overrides which
# values.yaml gets written (default: charts/$(BINARY_NAME)/values.yaml).
update-chart-digest:
ifdef DIGEST
	python3 hack/update-chart-digest.py --digest $(DIGEST) $(VALUES_FILE)
else ifdef IMAGE
	python3 hack/update-chart-digest.py --image $(IMAGE) $(VALUES_FILE)
else
	$(error Set DIGEST=sha256:<64hex> or IMAGE=<repo:tag> to pin $(VALUES_FILE)'s image.digest)
endif

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
	@echo "  compat-matrix-validate - Validate hack/compatibility-matrix.json against its JSON Schema"
	@echo "  compat-matrix    - Validate hack/compatibility-matrix.json has the required shape"
	@echo "  docker-build     - Build Docker image"
	@echo "  docker-push      - Build and push Docker image"
	@echo "  docker-buildx    - Build and push multi-arch image"
	@echo "  helm-lint        - Lint Helm chart"
	@echo "  helm-rbac-check  - Ensure rendered chart has no StorageClass RBAC"
	@echo "  helm-package     - Package Helm chart"
	@echo "  helm-install     - Install using Helm"
	@echo "  helm-uninstall   - Uninstall Helm release"
	@echo "  update-chart-digest - Pin charts/nfs-quota-agent's image.digest (DIGEST=sha256:<64hex> or IMAGE=<repo:tag>)"
	@echo "  verify-release   - Offline-verify a downloaded release bundle (RELEASE_DIR=...)"
	@echo "  release-preflight - Require Chart.yaml version/appVersion to match TAG=vX.Y.Z"
	@echo "  release-manifest-local - Produce a schemaVersion 4 release-manifest.json locally to exercise the schema"
	@echo "  release-bundle   - Build an offline/air-gap install tar.gz (IMAGE_REF=..., CHART_TGZ=... required)"
