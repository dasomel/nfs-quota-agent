#!/usr/bin/env bash
# Verify that release-manifest.json names the multi-arch image actually served
# by GHCR. Registry lookups are intentionally separate from verify-release.py,
# whose contract is offline verification of downloaded release assets.
set -euo pipefail

: "${TAG:?Set TAG to the release tag (for example v0.4.2)}"
MANIFEST="${MANIFEST:-release-manifest.json}"
REPOSITORY="${REPOSITORY:-ghcr.io/dasomel/nfs-quota-agent}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

test -f "$MANIFEST" || fail "manifest not found: $MANIFEST"
command -v jq >/dev/null 2>&1 || fail "jq is required"

manifest_tag=$(jq -er '.tag | strings' "$MANIFEST") || fail "manifest.tag is missing"
manifest_repository=$(jq -er '.image.repository | strings' "$MANIFEST") || fail "manifest.image.repository is missing"
manifest_digest=$(jq -er '.image.digest | strings' "$MANIFEST") || fail "manifest.image.digest is missing"

[[ "$manifest_tag" == "$TAG" ]] || fail "manifest tag $manifest_tag does not match TAG $TAG"
[[ "$manifest_repository" == "$REPOSITORY" ]] || fail "manifest repository $manifest_repository does not match REPOSITORY $REPOSITORY"
[[ "$manifest_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "manifest image digest is not sha256:<64 hex>: $manifest_digest"

if command -v crane >/dev/null 2>&1; then
  resolve_digest() { crane digest "$1"; }
  inspect_raw() { crane manifest "$1"; }
  inspector="crane"
elif docker buildx version >/dev/null 2>&1; then
  resolve_digest() { docker buildx imagetools inspect "$1" | awk '/^Digest:/ { print $2; exit }'; }
  inspect_raw() { docker buildx imagetools inspect --raw "$1"; }
  inspector="docker buildx imagetools"
else
  fail "need crane or docker buildx imagetools"
fi

resolve_tag() {
  local image_ref="$1"
  local digest
  digest=$(resolve_digest "$image_ref") || fail "could not resolve $image_ref with $inspector"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "invalid digest returned for $image_ref: $digest"
  printf '%s\n' "$digest"
}

verify_equals_manifest() {
  local tag="$1"
  local actual
  actual=$(resolve_tag "$REPOSITORY:$tag")
  echo "tag $REPOSITORY:$tag -> $actual"
  [[ "$actual" == "$manifest_digest" ]] || fail "$REPOSITORY:$tag resolved to $actual, expected $manifest_digest"
}

verify_equals_manifest "$TAG"

raw_manifest=$(inspect_raw "$REPOSITORY:$TAG") || fail "could not inspect raw manifest for $REPOSITORY:$TAG"
if ! jq -e '
  .manifests | type == "array" and
  any(.[]; .platform.os == "linux" and .platform.architecture == "amd64") and
  any(.[]; .platform.os == "linux" and .platform.architecture == "arm64") and
  any(.[]; .platform.os == "linux" and .platform.architecture == "arm" and .platform.variant == "v7")
' >/dev/null <<<"$raw_manifest"; then
  fail "$REPOSITORY:$TAG is not a multi-arch index containing linux/amd64, linux/arm64, and linux/arm/v7"
fi
echo "platforms: linux/amd64 linux/arm64 linux/arm/v7"

# Classify rc tags exactly the way the release workflow does (substring "-rc",
# see docker/metadata-action's `!contains(github.ref_name, '-rc')` gate and the
# release-manifest prerelease flag). A generic SemVer prerelease test would
# disagree with the pipeline for a hypothetical v0.4.2-beta1: build-and-push
# treats it as stable and moves the floating tags, so this check must too.
if [[ "$TAG" == *-rc* ]]; then
  # Derive the common floating tags from the release version so this remains
  # correct after the current 0.x line.
  version="${TAG#v}"
  IFS=. read -r major minor _ <<<"$version"
  for floating_tag in latest "$major.$minor" "$major"; do
    actual=$(resolve_tag "$REPOSITORY:$floating_tag")
    echo "floating tag $REPOSITORY:$floating_tag -> $actual"
    [[ "$actual" != "$manifest_digest" ]] || fail "RC digest $manifest_digest moved floating tag $REPOSITORY:$floating_tag"
  done
else
  version="${TAG#v}"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]] || fail "TAG is not a SemVer release: $TAG"
  IFS=. read -r major minor _ <<<"$version"
  for floating_tag in latest "$major.$minor" "$major"; do
    verify_equals_manifest "$floating_tag"
  done
fi

echo "OK: published digest verification passed for $TAG"
