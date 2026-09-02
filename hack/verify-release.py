#!/usr/bin/env python3
"""Offline verification for a downloaded nfs-quota-agent release bundle.

Cross-checks every artifact in a release directory (binaries, the Helm
chart, sbom.spdx.json, compatibility-matrix.json) against the sha256
digests recorded in release-manifest.json -- so a downloaded release can
be trusted without re-running the release pipeline (#16, #26).

Does not touch the network for the sha256 checks. Verifying the container
image itself needs a registry (`docker buildx imagetools inspect
<repository>@<digest>`); this script only prints that instruction rather
than attempting it, since pulling from a registry is a separate concern
from verifying the local artifact bundle.

release-manifest.json's ``sbom`` and ``compatibilityMatrix`` fields were
added in schemaVersion 2; a schemaVersion 1 manifest from an older release
is verified with those two checks skipped rather than failed. schemaVersion
3 additionally added a ``signatures`` field (cosign sign-blob bundles for
checksums.txt and the Helm chart) and, when present, the manifest's own
signature is expected alongside it as release-manifest.json.bundle; a
schemaVersion < 3 manifest, or one missing ``signatures``, has cosign
checks skipped rather than failed -- same backward-compatible pattern as
the sbom/compatibilityMatrix fields.

Cosign checks need the `cosign` CLI on PATH; if it is missing, those checks
are skipped with a clear message rather than failing the whole run. They
also need a locally pinned Sigstore trust root so verification of the
Fulcio cert chain / Rekor inclusion proof embedded in each bundle does not
require a network fetch of Sigstore's TUF trust material -- this script
defaults to hack/sigstore-trusted-root.json (shipped in this repo, next to
this script; refresh periodically with `cosign initialize` then copy
~/.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json over
it) and that default can be overridden with --trusted-root.

By default, any signature check that cannot run -- cosign missing, the
trusted root missing, or the manifest simply predating signatures
(schemaVersion < 3) -- is printed as SKIP and does not affect the exit
code, because the sha256 checks above already give a real integrity
signal on their own. Pass --require-signatures when that is not good
enough for your use case (e.g. verifying a release before it ships to
consumers who expect authenticity, not just integrity): every one of
those SKIP conditions then becomes a FAIL, and the script exits non-zero
instead of silently passing without having checked a signature at all.

An additive ``--bundle <path>`` mode (#5) verifies an offline/air-gap
install bundle (``make release-bundle``'s ``.tar.gz`` output) instead of a
release directory: the bundle archive's own sha256 (against
``release-manifest.json``'s ``bundle`` field when present, else a sidecar
``<bundle>.sha256`` -- neither is treated as a trust root by itself, see
below), a cosign signature check on BOTH ``release-manifest.json`` (its
already-published ``.bundle``) and the offline bundle itself (its own
``.bundle``, when the release-bundle job published one) honoring
``--trusted-root``/``--require-signatures`` exactly like the checks above,
that the OCI archive inside the bundle
(``images/nfs-quota-agent-image.tar``) has the same image digest the
manifest records, and that the chart ``.tgz`` inside the bundle matches
the manifest's recorded chart sha256. schemaVersion is not bumped for
this -- ``bundle`` is simply an optional field, same backward-compatible
pattern as ``sbom``/``compatibilityMatrix`` above. A sha256-only match
(sidecar or manifest field) without a passing cosign check is integrity,
not authenticity: an attacker who can replace release assets can replace
a sidecar checksum file alongside them, so the cosign signature -- not the
checksum -- is what actually answers "did this come from the real release
pipeline." It does not repeat the per-artifact binary/sbom checks above --
those apply to a full release directory, not a bundle, which intentionally
carries only the image, the chart, and the verifier itself.

Usage: hack/verify-release.py [release-dir] [--trusted-root PATH] [--require-signatures]
       hack/verify-release.py --bundle <bundle.tar.gz> [--manifest release-manifest.json]
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

SCRIPT_DIR = os.path.dirname(os.path.realpath(__file__))
DEFAULT_TRUSTED_ROOT = os.path.join(SCRIPT_DIR, "sigstore-trusted-root.json")

# The GitHub Actions workflow identity expected on every real (keyless)
# release signature -- matches release.yaml's own path so a bundle signed
# by a fork or a different workflow is rejected rather than accepted.
CERTIFICATE_IDENTITY_REGEXP = (
    r"^https://github\.com/dasomel/nfs-quota-agent/\.github/workflows/release\.yaml@refs/tags/.*$"
)
CERTIFICATE_OIDC_ISSUER = "https://token.actions.githubusercontent.com"

# Fields every real release-manifest.json has, regardless of schemaVersion
# -- release.yaml's jq call always sets all of these. A manifest missing
# any of them (a truncated/malformed file, or one crafted to make this
# script quietly check nothing and report OK) fails loudly here instead of
# producing a "0 checks ran, 0 errors, OK" result an operator could mistake
# for a real pass.
REQUIRED_TOP_LEVEL_FIELDS = ("tag", "sourceCommit", "workflowRun", "image", "chart", "binaries")

# The two per-artifact cosign sign-blob bundles release.yaml's "Generate
# release manifest" step always records under a schemaVersion-3 manifest's
# "signatures" field -- release.yaml:441-444's jq call sets exactly these
# two keys (checksums from the "Sign checksums" step at release.yaml:208,
# chart from the "Sign chart" step at release.yaml:320) every time it
# builds a real manifest, unconditionally. release-manifest.json's own
# signature (release-manifest.json.bundle, release.yaml:458) is not one of
# these -- it cannot record its own digest inside the manifest it signs,
# so it is checked directly by a fixed filename instead (see below), not
# looked up as a "signatures" entry. Under --require-signatures, a
# schemaVersion >= 3 manifest missing either of these two entries is exactly
# as suspicious as one missing REQUIRED_TOP_LEVEL_FIELDS above: a real
# release never produces one, so treating "signatures": {} or a
# partially-populated "signatures" as merely nothing-to-check would let a
# stripped-down manifest silently skip the very checks --require-signatures
# exists to make mandatory.
EXPECTED_SIGNATURE_ENTRIES = ("checksums", "chart")

# Extraction caps for --bundle (MEDIUM, Codex critic pass on #117): this
# bundle format is a handful of small-to-medium files (a chart .tgz, an
# OCI image archive, a few scripts/docs) -- nowhere near either limit for
# a legitimate build. A crafted bundle with an absurd member count or an
# extreme total uncompressed size (a tar-bomb: small compressed input,
# enormous expanded output) is refused outright before extraction rather
# than exhausting disk or memory partway through.
MAX_BUNDLE_MEMBERS = 10_000
MAX_BUNDLE_TOTAL_BYTES = 8 * 1024 ** 3  # 8 GiB
# Per-member cap (Codex final verification on #117): the total-size cap
# above bounds the sum across all members, but a single 8 GiB member would
# still pass it alone and fill the temp extraction disk before any other
# check runs. 4 GiB comfortably fits a real multi-arch image's largest
# single layer (this project's own image is tens of MB; even a
# heavyweight base-image layer in the wild is rarely more than a few GiB)
# while still being far below the 8 GiB total cap, so a legitimate bundle
# is never affected -- only a single implausibly large member is rejected.
MAX_BUNDLE_MEMBER_BYTES = 4 * 1024 ** 3  # 4 GiB


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def safe_join(release_dir, file_name, label, errors):
    """Joins release_dir with a manifest-supplied file name, refusing to
    resolve outside release_dir. file_name comes from release-manifest.json,
    which this script treats as untrusted input (see verify_manifest_shape) --
    without this check, an absolute path or a "../" value would let a
    crafted manifest make this script report a false "OK" for a file
    entirely outside the downloaded bundle."""
    if os.path.isabs(file_name):
        print(f"MISMATCH: {label}\n  reason: manifest file path {file_name!r} is absolute, refusing to resolve it")
        errors.append(label)
        return None
    release_root = os.path.realpath(release_dir)
    resolved = os.path.realpath(os.path.join(release_dir, file_name))
    if resolved != release_root and not resolved.startswith(release_root + os.sep):
        print(f"MISMATCH: {label}\n  reason: manifest file path {file_name!r} resolves outside {release_dir}")
        errors.append(label)
        return None
    return resolved


def check(label, release_dir, file_name, want, errors):
    path = safe_join(release_dir, file_name, label, errors)
    if path is None:
        return
    if not os.path.isfile(path):
        print(f"MISSING: {label} ({path} not present)")
        errors.append(label)
        return
    got = sha256_of(path)
    if got != want:
        print(f"MISMATCH: {label}\n  file:     {path}\n  expected: {want}\n  actual:   {got}")
        errors.append(label)
    else:
        print(f"OK: {label}")


def verify_cosign_bundle(label, release_dir, target_file, bundle_file, trusted_root, errors, require_signatures=False):
    """Verifies target_file against bundle_file with `cosign verify-blob`,
    pinned to this repo's release.yaml workflow identity and a locally
    supplied trusted root (see module docstring for why --trusted-root is
    required rather than left to cosign's default TUF fetch). Skips
    (without failing) when the `cosign` binary or the trusted root file
    isn't available -- this script's sha256 checks above already give a
    real integrity signal without cosign; the signature check is an
    additional authenticity signal, not the only line of defense --
    unless require_signatures is set, in which case a consumer explicitly
    asked for that authenticity signal and an unrunnable check must fail
    loudly rather than silently pass with less than what was asked for."""
    if shutil.which("cosign") is None:
        if require_signatures:
            print(f"FAIL: {label} signature (cosign not found on PATH)")
            errors.append(f"{label} signature")
        else:
            print(f"SKIP: {label} signature (cosign not found on PATH)")
        return
    if not os.path.isfile(trusted_root):
        if require_signatures:
            print(f"FAIL: {label} signature (trusted root not found: {trusted_root})")
            errors.append(f"{label} signature")
        else:
            print(f"SKIP: {label} signature (trusted root not found: {trusted_root})")
        return
    target_path = safe_join(release_dir, target_file, f"{label} signature", errors)
    bundle_path = safe_join(release_dir, bundle_file, f"{label} signature", errors)
    if target_path is None or bundle_path is None:
        return
    if not os.path.isfile(target_path):
        print(f"MISSING: {label} signature ({target_path} not present)")
        errors.append(f"{label} signature")
        return
    if not os.path.isfile(bundle_path):
        print(f"MISSING: {label} signature ({bundle_path} not present)")
        errors.append(f"{label} signature")
        return
    result = subprocess.run(
        [
            "cosign", "verify-blob",
            "--bundle", bundle_path,
            "--trusted-root", trusted_root,
            "--certificate-identity-regexp", CERTIFICATE_IDENTITY_REGEXP,
            "--certificate-oidc-issuer", CERTIFICATE_OIDC_ISSUER,
            target_path,
        ],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print(f"MISMATCH: {label} signature\n{result.stdout}{result.stderr}")
        errors.append(f"{label} signature")
    else:
        print(f"OK: {label} signature")


def verify_cosign_bundle_abs(label, target_path, bundle_path, trusted_root, errors, require_signatures=False):
    """Same cosign verify-blob check as verify_cosign_bundle(), for callers
    (bundle mode) that already hold absolute, locally-resolved paths --
    e.g. a --bundle/--manifest CLI argument, or a path returned from
    extracting a downloaded archive -- rather than a manifest-supplied file
    name that needs safe_join()'s traversal guard. Duplicated instead of
    reusing safe_join() because there is no single release_dir these two
    paths are relative to: target_path may be the bundle argument itself
    while bundle_path is a signature file next to it, or a manifest_path
    next to a differently-located release-manifest.json."""
    if shutil.which("cosign") is None:
        if require_signatures:
            print(f"FAIL: {label} signature (cosign not found on PATH)")
            errors.append(f"{label} signature")
        else:
            print(f"SKIP: {label} signature (cosign not found on PATH)")
        return
    if not os.path.isfile(trusted_root):
        if require_signatures:
            print(f"FAIL: {label} signature (trusted root not found: {trusted_root})")
            errors.append(f"{label} signature")
        else:
            print(f"SKIP: {label} signature (trusted root not found: {trusted_root})")
        return
    if not os.path.isfile(target_path):
        if require_signatures:
            print(f"FAIL: {label} signature ({target_path} not present)")
            errors.append(f"{label} signature")
        else:
            print(f"SKIP: {label} signature ({target_path} not present)")
        return
    if not os.path.isfile(bundle_path):
        if require_signatures:
            print(f"FAIL: {label} signature ({bundle_path} not present)")
            errors.append(f"{label} signature")
        else:
            print(f"SKIP: {label} signature ({bundle_path} not present)")
        return
    result = subprocess.run(
        [
            "cosign", "verify-blob",
            "--bundle", bundle_path,
            "--trusted-root", trusted_root,
            "--certificate-identity-regexp", CERTIFICATE_IDENTITY_REGEXP,
            "--certificate-oidc-issuer", CERTIFICATE_OIDC_ISSUER,
            target_path,
        ],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print(f"MISMATCH: {label} signature\n{result.stdout}{result.stderr}")
        errors.append(f"{label} signature")
    else:
        print(f"OK: {label} signature")


MAX_BLOB_WALK_SIZE_BYTES = 4 * 1024 * 1024  # cap for JSON blobs (index/manifest/config); layers are hashed but never fully parsed


def _sha256_of_tar_member(tf, member):
    """Streams a tar member's content through sha256 in fixed-size chunks
    (never loading a whole layer blob into memory at once -- Codex's HIGH
    finding on the prior version of this function, which only compared the
    descriptor *string* in index.json and never touched blobs/sha256/* at
    all, so a tampered or entirely missing blob passed silently)."""
    f = tf.extractfile(member)
    if f is None:
        return None
    h = hashlib.sha256()
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
    return h.hexdigest()


def _oci_blob_member(tf, digest, errors, context):
    """Resolves an OCI descriptor's "sha256:<hex>" digest to its
    blobs/sha256/<hex> tar member, verifies the member's actual content
    hashes to that same digest (the real integrity check -- a descriptor
    is just a claim; the blob's own bytes are the evidence), and returns
    the member. Returns None (having already recorded an error) on any
    failure: missing "sha256:" prefix, missing blob file, or a hash
    mismatch (a tampered blob)."""
    if not digest or not digest.startswith("sha256:"):
        print(f"FAIL: bundle image ({context}: descriptor digest {digest!r} is not a sha256: digest)")
        errors.append("bundle image")
        return None
    hexdigest = digest[len("sha256:"):]
    blob_path = f"blobs/sha256/{hexdigest}"
    try:
        member = tf.getmember(blob_path)
    except KeyError:
        print(f"FAIL: bundle image ({context}: blob {blob_path} not present in the OCI archive)")
        errors.append("bundle image")
        return None
    got = _sha256_of_tar_member(tf, member)
    if got != hexdigest:
        print(f"MISMATCH: bundle image ({context}: blob {blob_path} content does not hash to its own digest -- expected {hexdigest}, actual {got})")
        errors.append("bundle image")
        return None
    return member


def _oci_blob_json(tf, digest, errors, context):
    """_oci_blob_member() plus parsing the (hash-verified) blob as JSON --
    for index/manifest/config blobs, which are small and always JSON.
    Layer blobs are never parsed this way (see MAX_BLOB_WALK_SIZE_BYTES);
    they are only hash-verified via _oci_blob_member()."""
    member = _oci_blob_member(tf, digest, errors, context)
    if member is None:
        return None
    if member.size > MAX_BLOB_WALK_SIZE_BYTES:
        print(f"FAIL: bundle image ({context}: blob for digest {digest} is {member.size} bytes, larger than the {MAX_BLOB_WALK_SIZE_BYTES}-byte cap expected for an index/manifest/config JSON blob -- refusing to parse it as JSON)")
        errors.append("bundle image")
        return None
    with tf.extractfile(member) as f:
        try:
            return json.load(f)
        except json.JSONDecodeError as exc:
            print(f"FAIL: bundle image ({context}: blob for digest {digest} is not valid JSON: {exc})")
            errors.append("bundle image")
            return None


def oci_archive_image_digest(oci_tar_path, errors):
    """Verifies an OCI archive (produced by `skopeo copy --all ... oci:...`
    or `docker buildx --output type=oci`) end to end and returns its
    top-level index digest, or None if any check below failed (an error
    is always appended to `errors` in that case).

    This walks every layer of the archive's content-addressed structure,
    hash-verifying each blob against its own claimed digest as it goes
    (Codex's HIGH finding on the prior version: comparing only the
    descriptor *string* in index.json, without ever touching
    blobs/sha256/*, means a tampered or entirely missing blob passes
    silently):

      1. Root oci-layout index.json's manifests[0] descriptor digest --
         resolved and hash-verified against blobs/sha256/<digest> (this is
         the value returned, and what release-manifest.json's image.digest
         is compared against elsewhere in this script -- see the --all
         rationale below).
      2. That blob's own content, when it is itself an image index
         (mediaType "application/vnd.oci.image.index.v1+json" -- the
         multi-arch case `--all` produces): each platform's manifest
         descriptor is resolved and hash-verified the same way.
      3. Each platform manifest's config and every layer descriptor:
         resolved and hash-verified (streamed -- see
         MAX_BLOB_WALK_SIZE_BYTES and _sha256_of_tar_member) but never
         parsed as JSON for layers, since a layer is often a large
         compressed tarball, not JSON.

      A tampered blob, a missing blob, or a descriptor digest edited to
      not match its own referenced blob's real hash all now surface as a
      FAIL naming exactly which blob and why, instead of silently passing.

    --all matters: release-manifest.json's image.digest is the multi-arch
    manifest LIST digest reported by docker/build-push-action, i.e. the
    digest of the raw index blob a registry serves for that tag (verified
    directly: `skopeo copy --all docker://.../alpine:3.24 oci:dir:latest`
    then dir/index.json's manifests[0].digest -- when its mediaType is
    itself "application/vnd.oci.image.index.v1+json" -- equals sha256 of
    `skopeo inspect --raw docker://.../alpine:3.24`, byte for byte). Without
    --all, skopeo copies only the host-arch manifest, so this function
    would return a per-arch manifest digest instead of the index digest --
    a guaranteed mismatch against a real multi-arch release-manifest.json,
    which is exactly the CRITICAL-3 defect this comment documents so it
    doesn't get "fixed" by loosening the comparison instead of keeping
    --all in the Makefile."""
    try:
        with tarfile.open(oci_tar_path, "r") as tf:
            try:
                root_member = tf.getmember("index.json")
            except KeyError:
                print(f"FAIL: bundle image ({oci_tar_path} has no top-level index.json)")
                errors.append("bundle image")
                return None
            with tf.extractfile(root_member) as f:
                root_index = json.load(f)

            manifests = root_index.get("manifests") or []
            if not manifests or "digest" not in manifests[0]:
                print(f"FAIL: bundle image ({oci_tar_path}'s index.json has no manifest digest)")
                errors.append("bundle image")
                return None
            top_digest = manifests[0]["digest"]
            top_media_type = manifests[0].get("mediaType", "")

            errors_before = len(errors)
            top_member = _oci_blob_member(tf, top_digest, errors, "top-level index descriptor")
            if top_member is None:
                return None

            if top_media_type == "application/vnd.oci.image.index.v1+json":
                nested_index = _oci_blob_json(tf, top_digest, errors, "multi-arch index")
                if nested_index is None:
                    return None
                platform_manifests = nested_index.get("manifests") or []
                if not platform_manifests:
                    print(f"FAIL: bundle image (multi-arch index for digest {top_digest} lists no platform manifests)")
                    errors.append("bundle image")
                    return None
                for platform_entry in platform_manifests:
                    platform_digest = platform_entry.get("digest")
                    platform_desc = platform_entry.get("platform") or {}
                    label = f"platform manifest ({platform_desc.get('os', '?')}/{platform_desc.get('architecture', '?')})"
                    manifest_obj = _oci_blob_json(tf, platform_digest, errors, label)
                    if manifest_obj is None:
                        continue
                    config = manifest_obj.get("config") or {}
                    if config.get("digest"):
                        _oci_blob_member(tf, config["digest"], errors, f"{label} config")
                    for layer in manifest_obj.get("layers") or []:
                        if layer.get("digest"):
                            _oci_blob_member(tf, layer["digest"], errors, f"{label} layer")
            elif top_media_type == "application/vnd.oci.image.manifest.v1+json":
                # Single-platform archive (e.g. the docker-save/docker-archive
                # local dev path, which has no multi-arch index -- see the
                # Makefile's WARNING on that branch). Walk it the same way a
                # platform manifest is walked above.
                manifest_obj = _oci_blob_json(tf, top_digest, errors, "manifest")
                if manifest_obj is not None:
                    config = manifest_obj.get("config") or {}
                    if config.get("digest"):
                        _oci_blob_member(tf, config["digest"], errors, "manifest config")
                    for layer in manifest_obj.get("layers") or []:
                        if layer.get("digest"):
                            _oci_blob_member(tf, layer["digest"], errors, "manifest layer")
            # else: unknown mediaType -- the top-level blob itself is still
            # hash-verified above; nothing further to walk.

            if len(errors) > errors_before:
                return None
            return top_digest
    except (tarfile.TarError, OSError) as exc:
        print(f"FAIL: bundle image (could not read {oci_tar_path}: {exc})")
        errors.append("bundle image")
        return None


def verify_bundle(bundle_path, manifest_path, trusted_root, errors, require_signatures=False):
    """Verifies an offline install bundle (#5) built by `make
    release-bundle`: the bundle archive's own sha256, its cosign signature
    (and the release manifest's), the OCI archive's image digest against
    the manifest's image.digest, and the chart .tgz inside the bundle
    against the manifest's chart.sha256.

    Authentication, not just integrity (CRITICAL-1 from an independent
    review of #117): a sidecar <bundle>.sha256 file is NOT treated as a
    trust root by itself -- anyone who can replace the bundle can replace
    its sidecar checksum file too, so it is only ever a convenience
    cross-check. The actual authority is a cosign signature: this function
    verifies release-manifest.json.bundle (the already-published manifest
    signature) and, when the release-bundle job published one,
    <bundle>.bundle, exactly like the non-bundle code path does for
    checksums.txt/the chart -- including honoring --require-signatures
    (absent cosign/trusted-root/signature-file becomes FAIL, not a silent
    pass, when the caller asked for that).

    Extracts the bundle to a temp directory rather than reading tar
    members individually so the chart-digest and image-digest checks can
    reuse the same sha256_of()/oci_archive_image_digest() helpers used
    elsewhere in this script. Extraction guards against a symlink-escape
    tar (CRITICAL-2): before extracting anything, every member is checked
    for issym()/islnk()/isdev() (rejected outright -- this bundle format
    never legitimately contains one) and for an absolute or realpath-
    escaping name; on Python >= 3.12, tarfile's own filter="data" is passed
    too, belt-and-braces, since a hand-crafted member ordering (a symlink
    created before the member that walks through it) can defeat a purely
    pre-extraction realpath check that resolves paths that don't exist yet."""
    if not os.path.isfile(bundle_path):
        print(f"FAIL: bundle not found: {bundle_path}", file=sys.stderr)
        errors.append("bundle")
        return

    manifest = None
    if manifest_path and os.path.isfile(manifest_path):
        with open(manifest_path) as f:
            manifest = json.load(f)

    # -- 1. Bundle's own sha256 (manifest field, else sidecar; see docstring) --
    want_bundle_sha256 = None
    bundle_sha256_source = None
    if manifest is not None:
        bundle_entry = manifest.get("bundle")
        if bundle_entry and bundle_entry.get("sha256"):
            want_bundle_sha256 = bundle_entry["sha256"]
            bundle_sha256_source = "release-manifest.json 'bundle' field"
    if want_bundle_sha256 is None:
        sidecar = bundle_path + ".sha256"
        if os.path.isfile(sidecar):
            sidecar_text = Path(sidecar).read_text()
            first_token = sidecar_text.split()[0] if sidecar_text.strip() else ""
            if first_token:
                want_bundle_sha256 = first_token
                bundle_sha256_source = os.path.basename(sidecar) + " (convenience check only, not a trust root -- see cosign checks below)"

    if want_bundle_sha256:
        got = sha256_of(bundle_path)
        if got != want_bundle_sha256:
            print(
                f"MISMATCH: bundle archive\n  file:     {bundle_path}\n"
                f"  expected: {want_bundle_sha256} (from {bundle_sha256_source})\n  actual:   {got}"
            )
            errors.append("bundle archive")
        else:
            print(f"OK: bundle archive {bundle_path} (checked against {bundle_sha256_source})")
    else:
        print("SKIP: bundle archive sha256 (no release-manifest.json 'bundle' field or <bundle>.sha256 sidecar found)")

    # -- 2. Cosign signatures: the manifest's own, and the bundle's own -- authority, not the sidecar above.
    errors_before_signatures = len(errors)
    if manifest_path and os.path.isfile(manifest_path):
        verify_cosign_bundle_abs(
            "release-manifest.json",
            manifest_path,
            manifest_path + ".bundle",
            trusted_root,
            errors,
            require_signatures,
        )
    else:
        if require_signatures:
            print("FAIL: release-manifest.json signature (no release-manifest.json given to verify)")
            errors.append("release-manifest.json signature")
        else:
            print("SKIP: release-manifest.json signature (no release-manifest.json given to verify)")

    verify_cosign_bundle_abs(
        "bundle",
        bundle_path,
        bundle_path + ".bundle",
        trusted_root,
        errors,
        require_signatures,
    )

    if require_signatures and len(errors) > errors_before_signatures:
        # MEDIUM (Codex critic pass on #117): when the caller demanded
        # signature verification and it failed, do not go on to open and
        # extract a bundle whose authenticity was just rejected -- there is
        # no reason to spend effort (or risk a decompression-bomb-style
        # resource cap issue) parsing content that has already failed the
        # only check that matters when --require-signatures was asked for.
        print("SKIP: bundle contents (signature verification failed above under --require-signatures; not opening the archive)")
        return

    # -- 3. Contents: image digest and chart sha256 against the manifest --
    with tempfile.TemporaryDirectory() as raw_tmp:
        tmp = os.path.realpath(raw_tmp)
        try:
            with tarfile.open(bundle_path, "r:*") as tf:
                # Manual guard (CRITICAL-2), belt-and-braces with
                # filter="data" below on interpreters that support it: a
                # pre-extraction realpath check alone is defeatable by a
                # symlink member extracted before the member that walks
                # through it (the symlink target doesn't exist yet when
                # the *later* member's path is resolved, so it can
                # resolve as "inside tmp" even though the symlink makes it
                # land outside once both are on disk). Rejecting every
                # symlink/hardlink/device member outright closes that:
                # this bundle format never legitimately contains one.
                #
                # MEDIUM (Codex critic pass on #117): also caps member
                # count, per-member size, and total uncompressed size
                # before extracting anything, so a maliciously crafted
                # bundle (a tar-bomb -- a small compressed file that
                # expands to an enormous number of members, or a single
                # enormous member) fails with a clear message instead of
                # exhausting disk/memory during extraction. The per-member
                # cap matters on its own: a single member under the total
                # cap could still exhaust the temp extraction disk before
                # any other member is even looked at.
                safe_members = []
                total_size = 0
                for member in tf.getmembers():
                    if member.issym() or member.islnk() or member.isdev():
                        print(f"FAIL: bundle contents (member {member.name!r} is a symlink/hardlink/device, not allowed in this bundle format)")
                        errors.append("bundle contents")
                        return
                    resolved = os.path.realpath(os.path.join(tmp, member.name))
                    if os.path.isabs(member.name) or not (resolved == tmp or resolved.startswith(tmp + os.sep)):
                        print(f"FAIL: bundle contents (member {member.name!r} escapes the extraction directory)")
                        errors.append("bundle contents")
                        return
                    if len(safe_members) + 1 > MAX_BUNDLE_MEMBERS:
                        print(f"FAIL: bundle contents (more than {MAX_BUNDLE_MEMBERS} members -- refusing to extract, this bundle format never legitimately has that many)")
                        errors.append("bundle contents")
                        return
                    if member.size > MAX_BUNDLE_MEMBER_BYTES:
                        print(f"FAIL: bundle contents (member {member.name!r} claims {member.size} bytes, larger than the {MAX_BUNDLE_MEMBER_BYTES}-byte per-member cap -- refusing to extract, possible tar-bomb)")
                        errors.append("bundle contents")
                        return
                    total_size += max(member.size, 0)
                    if total_size > MAX_BUNDLE_TOTAL_BYTES:
                        print(f"FAIL: bundle contents (total uncompressed size exceeds {MAX_BUNDLE_TOTAL_BYTES} bytes -- refusing to extract, possible tar-bomb)")
                        errors.append("bundle contents")
                        return
                    safe_members.append(member)
                extract_kwargs = {"members": safe_members}
                if hasattr(tarfile, "data_filter"):
                    # Python >= 3.12: also apply tarfile's own hardened
                    # filter, which additionally normalizes permissions
                    # and re-checks link targets at extraction time rather
                    # than only at the pre-scan above.
                    extract_kwargs["filter"] = "data"
                tf.extractall(tmp, **extract_kwargs)
        except (tarfile.TarError, OSError) as exc:
            print(f"FAIL: bundle contents (could not extract {bundle_path}: {exc})")
            errors.append("bundle contents")
            return

        image_tar = os.path.join(tmp, "images", "nfs-quota-agent-image.tar")
        if not os.path.isfile(image_tar):
            print(f"MISSING: bundle image ({image_tar} not present in bundle)")
            errors.append("bundle image")
        else:
            image_digest = oci_archive_image_digest(image_tar, errors)
            if image_digest is not None and manifest is not None:
                want_digest = (manifest.get("image") or {}).get("digest")
                if want_digest:
                    if image_digest != want_digest:
                        print(
                            f"MISMATCH: bundle image digest\n  expected: {want_digest}\n  actual:   {image_digest}"
                        )
                        errors.append("bundle image digest")
                    else:
                        print(f"OK: bundle image digest {image_digest}")
                else:
                    print("SKIP: bundle image digest (release-manifest.json has no image.digest)")
            elif image_digest is not None:
                print(f"NOTE: bundle image digest {image_digest} (no release-manifest.json given to compare against)")

        chart_dir = os.path.join(tmp, "chart")
        chart_files = sorted(f for f in os.listdir(chart_dir) if f.endswith(".tgz")) if os.path.isdir(chart_dir) else []
        if not chart_files:
            print(f"MISSING: bundle chart (no .tgz found under {chart_dir})")
            errors.append("bundle chart")
        elif len(chart_files) > 1:
            print(f"FAIL: bundle chart (expected exactly one .tgz under {chart_dir}, found {len(chart_files)}: {chart_files})")
            errors.append("bundle chart")
        else:
            chart_path = os.path.join(chart_dir, chart_files[0])
            got = sha256_of(chart_path)
            if manifest is not None:
                want = (manifest.get("chart") or {}).get("sha256")
                if want:
                    if got != want:
                        print(
                            f"MISMATCH: bundle chart {chart_files[0]}\n  expected: {want}\n  actual:   {got}"
                        )
                        errors.append("bundle chart")
                    else:
                        print(f"OK: bundle chart {chart_files[0]}")
                else:
                    print(f"SKIP: bundle chart sha256 (release-manifest.json has no chart.sha256)")
            else:
                print(f"NOTE: bundle chart {chart_files[0]} sha256 {got} (no release-manifest.json given to compare against)")



def verify_manifest_shape(manifest):
    """Returns a list of shape problems (missing/malformed required fields)
    without touching the filesystem. release-manifest.json is downloaded
    alongside the artifacts it describes, so a truncated or hand-crafted
    manifest is exactly as untrusted as the artifacts themselves -- a
    manifest missing "binaries" or "chart" entirely must not be allowed to
    make every artifact check silently no-op into a false OK."""
    problems = []
    for field in REQUIRED_TOP_LEVEL_FIELDS:
        if field not in manifest:
            problems.append(f"missing required field {field!r}")

    image = manifest.get("image")
    if isinstance(image, dict):
        if not image.get("repository") or not image.get("digest"):
            problems.append("image.repository and image.digest must both be set")
    elif "image" in manifest:
        problems.append("image must be an object")

    chart = manifest.get("chart")
    if isinstance(chart, dict):
        if not chart.get("file") or not chart.get("sha256"):
            problems.append("chart.file and chart.sha256 must both be set")
    elif "chart" in manifest:
        problems.append("chart must be an object")

    binaries = manifest.get("binaries")
    if isinstance(binaries, list):
        if not binaries:
            problems.append("binaries must not be empty")
        for i, b in enumerate(binaries):
            if not isinstance(b, dict) or not b.get("file") or not b.get("sha256"):
                problems.append(f"binaries[{i}] must be an object with file and sha256 set")
    elif "binaries" in manifest:
        problems.append("binaries must be a list")

    return problems


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("release_dir", nargs="?", default=".", help="directory holding the downloaded release artifacts")
    parser.add_argument(
        "--bundle",
        metavar="PATH",
        help="verify an offline install bundle (make release-bundle's .tar.gz) instead of a release directory",
    )
    parser.add_argument(
        "--manifest",
        metavar="PATH",
        help="release-manifest.json to check --bundle against (default: release-manifest.json next to the bundle, if present)",
    )
    parser.add_argument(
        "--trusted-root",
        default=DEFAULT_TRUSTED_ROOT,
        help=f"path to a pinned Sigstore trusted_root.json (default: {DEFAULT_TRUSTED_ROOT})",
    )
    parser.add_argument(
        "--require-signatures",
        action="store_true",
        help=(
            "treat every skipped signature check (cosign missing, trusted root "
            "missing, or a release-manifest.json predating signatures) as a "
            "verification failure instead of a SKIP; use this when consumers "
            "of the verification need authenticity guaranteed, not just integrity"
        ),
    )
    args = parser.parse_args()

    if args.bundle:
        errors = []
        manifest_path = args.manifest
        manifest_source = "from --manifest" if manifest_path else None
        if manifest_path is None:
            candidate = os.path.join(os.path.dirname(os.path.realpath(args.bundle)), "release-manifest.json")
            if os.path.isfile(candidate):
                manifest_path = candidate
                manifest_source = "auto-discovered next to bundle"
        if manifest_path is None or not os.path.isfile(manifest_path):
            # MEDIUM (Codex final verification on #117): a manifest-less
            # "OK" was possible before this check -- --bundle with no
            # --manifest and no release-manifest.json auto-discovered next
            # to it skipped every cross-check and still exited 0, which a
            # user could easily mistake for "verified" rather than "nothing
            # was actually checked." release-manifest.json is REQUIRED now:
            # pass --manifest explicitly (fetched, like the bundle itself,
            # from the release page) if it isn't sitting next to the bundle.
            print(
                f"FAIL: no release-manifest.json found -- pass --manifest PATH "
                f"(download release-manifest.json from the same release page "
                f"as the bundle) or place it next to {args.bundle}",
                file=sys.stderr,
            )
            return 1
        # Decision D (Codex delta re-check on #117): auto-discovery of a
        # sibling release-manifest.json stays -- it goes through the exact
        # same cosign signature check as an explicitly-passed --manifest,
        # so it is not a trust hole by itself. What auto-discovery must
        # never do is verify silently: printing which one was used and how
        # it was found makes the source of trust visible in the output
        # either way, so a reviewer of a verification log (or CI output)
        # can tell "signed, and I know which manifest" from "signed,
        # against a file I didn't realize was picked up automatically."
        print(f"manifest: {manifest_path} ({manifest_source})")
        verify_bundle(args.bundle, manifest_path, args.trusted_root, errors, args.require_signatures)
        print()
        if errors:
            sys.stdout.flush()
            print(f"FAIL: {len(errors)} check(s) failed: {', '.join(errors)}", file=sys.stderr)
            return 1
        print(f"OK: bundle {args.bundle} verified against {manifest_path}")
        return 0

    release_dir = args.release_dir
    manifest_path = os.path.join(release_dir, "release-manifest.json")
    if not os.path.isfile(manifest_path):
        print(f"FAIL: release-manifest.json not found in {release_dir}", file=sys.stderr)
        return 1

    with open(manifest_path) as f:
        manifest = json.load(f)

    shape_problems = verify_manifest_shape(manifest)
    if shape_problems:
        print(f"FAIL: {manifest_path} is not a well-formed release manifest:", file=sys.stderr)
        for problem in shape_problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    errors = []
    schema_version = manifest.get("schemaVersion", 0)

    for binary in manifest.get("binaries", []):
        check(f"binary {binary['file']}", release_dir, binary["file"], binary["sha256"], errors)

    chart = manifest.get("chart")
    check(f"chart {chart['file']}", release_dir, chart["file"], chart["sha256"], errors)

    sbom = manifest.get("sbom")
    if sbom:
        check(f"sbom {sbom['file']}", release_dir, sbom["file"], sbom["sha256"], errors)
    elif schema_version < 2:
        print("SKIP: sbom (release-manifest schemaVersion < 2, no sbom digest recorded)")

    compat = manifest.get("compatibilityMatrix")
    if compat:
        check(
            f"compatibilityMatrix {compat['file']}",
            release_dir,
            compat["file"],
            compat["sha256"],
            errors,
        )
    elif schema_version < 2:
        print("SKIP: compatibilityMatrix (release-manifest schemaVersion < 2, not recorded)")

    signatures = manifest.get("signatures")
    if args.require_signatures and schema_version >= 3:
        for entry_name in EXPECTED_SIGNATURE_ENTRIES:
            if not (isinstance(signatures, dict) and signatures.get(entry_name)):
                print(f"FAIL: {entry_name} signature entry missing from release-manifest.json")
                errors.append(f"{entry_name} signature entry")

    if signatures:
        checksums_sig = signatures.get("checksums")
        if checksums_sig:
            check(
                "checksums.txt bundle",
                release_dir,
                checksums_sig["file"],
                checksums_sig["sha256"],
                errors,
            )
            verify_cosign_bundle(
                "checksums.txt",
                release_dir,
                "checksums.txt",
                checksums_sig["file"],
                args.trusted_root,
                errors,
                args.require_signatures,
            )

        chart_sig = signatures.get("chart")
        if chart_sig:
            check(f"{chart['file']} bundle", release_dir, chart_sig["file"], chart_sig["sha256"], errors)
            verify_cosign_bundle(
                f"chart {chart['file']}",
                release_dir,
                chart["file"],
                chart_sig["file"],
                args.trusted_root,
                errors,
                args.require_signatures,
            )

        # release-manifest.json.bundle signs this manifest file itself, so
        # it cannot record its own sha256 inside the manifest it signs --
        # its filename is a fixed convention from release.yaml rather than
        # a manifest field, checked directly with cosign instead of check().
        verify_cosign_bundle(
            "release-manifest.json",
            release_dir,
            "release-manifest.json",
            "release-manifest.json.bundle",
            args.trusted_root,
            errors,
            args.require_signatures,
        )
    elif schema_version < 3:
        if args.require_signatures:
            print("FAIL: signatures (release-manifest schemaVersion < 3, no signature bundles recorded)")
            errors.append("signatures")
        else:
            print("SKIP: signatures (release-manifest schemaVersion < 3, no signature bundles recorded)")

    image = manifest.get("image", {})
    print(
        f"NOT VERIFIED (needs registry access): image {image['repository']}@{image['digest']}\n"
        f"  Run: docker buildx imagetools inspect {image['repository']}@{image['digest']}"
    )

    print()
    if errors:
        sys.stdout.flush()
        print(f"FAIL: {len(errors)} artifact(s) failed verification: {', '.join(errors)}", file=sys.stderr)
        return 1

    print(
        f"OK: release {manifest.get('tag', '?')} "
        f"(source commit {manifest.get('sourceCommit', '?')}) verified against {manifest_path}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
