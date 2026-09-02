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
release directory: it checks the bundle archive's own sha256 against
``release-manifest.json``'s ``bundle`` field when present (schemaVersion
is not bumped for this -- ``bundle`` is simply an optional field, same
backward-compatible pattern as ``sbom``/``compatibilityMatrix`` above),
that the OCI archive inside the bundle (``images/nfs-quota-agent-image.tar``)
has the same image digest the manifest records, and that the chart
``.tgz`` inside the bundle matches the manifest's recorded chart sha256.
It does not repeat the per-artifact binary/sbom/signature checks above --
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


def oci_archive_image_digest(oci_tar_path, errors):
    """Reads an OCI archive's (produced by `skopeo copy ... oci-archive:...`
    or `docker buildx --output type=oci`) top-level index.json and returns
    the digest of its first manifest entry -- for a single-platform or
    already-digest-pinned source (which `make release-bundle` requires via
    IMAGE_REF), that is the same digest release-manifest.json records under
    image.digest, so this is a direct string comparison rather than a
    registry pull. Returns None (and appends to errors) if index.json is
    missing or malformed."""
    try:
        with tarfile.open(oci_tar_path, "r") as tf:
            member = tf.getmember("index.json")
            with tf.extractfile(member) as f:
                index = json.load(f)
    except (KeyError, tarfile.TarError, json.JSONDecodeError, OSError) as exc:
        print(f"FAIL: bundle image (could not read index.json from {oci_tar_path}: {exc})")
        errors.append("bundle image")
        return None
    manifests = index.get("manifests") or []
    if not manifests or "digest" not in manifests[0]:
        print(f"FAIL: bundle image ({oci_tar_path}'s index.json has no manifest digest)")
        errors.append("bundle image")
        return None
    return manifests[0]["digest"]


def verify_bundle(bundle_path, manifest_path, errors):
    """Verifies an offline install bundle (#5) built by `make
    release-bundle`: the bundle archive's own sha256 against a known-good
    value, the OCI archive's image digest against the manifest's
    image.digest, and the chart .tgz inside the bundle against the
    manifest's chart.sha256. Extracts the bundle to a temp directory rather
    than reading tar members individually so the chart-digest and
    image-digest checks can reuse the same sha256_of()/oci_archive_image_digest()
    helpers used elsewhere in this script.

    The bundle's own sha256 is checked against whichever of two sources is
    available, in this order: (1) release-manifest.json's optional "bundle"
    field -- not present on a real release yet, since release.yaml's
    release-bundle job appends after release-manifest is already signed and
    published, and re-signing that manifest to add a field after the fact
    would invalidate its existing signature; or (2) a sidecar
    ``<bundle>.sha256`` file next to the bundle -- the same
    one-line-checksum convention release.yaml already uses for the Helm
    chart (``<chart>.tgz.sha256``), published by that job instead."""
    if not os.path.isfile(bundle_path):
        print(f"FAIL: bundle not found: {bundle_path}", file=sys.stderr)
        errors.append("bundle")
        return

    manifest = None
    if manifest_path and os.path.isfile(manifest_path):
        with open(manifest_path) as f:
            manifest = json.load(f)

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
            # Same "<hash>  <filename>" format sha256sum/checksums.txt use
            # elsewhere in this repo.
            first_token = Path(sidecar).read_text().split()[0] if Path(sidecar).read_text().strip() else ""
            if first_token:
                want_bundle_sha256 = first_token
                bundle_sha256_source = os.path.basename(sidecar)

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

    with tempfile.TemporaryDirectory() as raw_tmp:
        tmp = os.path.realpath(raw_tmp)
        try:
            with tarfile.open(bundle_path, "r:*") as tf:
                # Manual path-traversal guard instead of tarfile's `filter="data"`
                # (Python 3.12+ only; this repo's hack/ scripts stay compatible
                # with older stdlib per hack/validate-compatibility-matrix.py's
                # documented convention) -- same untrusted-input posture as
                # safe_join() above, applied to a downloaded bundle instead of
                # a manifest-supplied file name.
                safe_members = []
                for member in tf.getmembers():
                    resolved = os.path.realpath(os.path.join(tmp, member.name))
                    if os.path.isabs(member.name) or not (resolved == tmp or resolved.startswith(tmp + os.sep)):
                        print(f"FAIL: bundle contents (member {member.name!r} escapes the extraction directory)")
                        errors.append("bundle contents")
                        return
                    safe_members.append(member)
                tf.extractall(tmp, members=safe_members)
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
        chart_files = [f for f in os.listdir(chart_dir) if f.endswith(".tgz")] if os.path.isdir(chart_dir) else []
        if not chart_files:
            print(f"MISSING: bundle chart (no .tgz found under {chart_dir})")
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
        if manifest_path is None:
            candidate = os.path.join(os.path.dirname(os.path.realpath(args.bundle)), "release-manifest.json")
            if os.path.isfile(candidate):
                manifest_path = candidate
        verify_bundle(args.bundle, manifest_path, errors)
        print()
        if errors:
            sys.stdout.flush()
            print(f"FAIL: {len(errors)} check(s) failed: {', '.join(errors)}", file=sys.stderr)
            return 1
        print(f"OK: bundle {args.bundle} verified" + (f" against {manifest_path}" if manifest_path else " (no manifest to compare against)"))
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
