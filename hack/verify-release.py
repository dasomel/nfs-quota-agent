#!/usr/bin/env python3
"""Offline verification for a downloaded nfs-quota-agent release bundle.

Cross-checks every artifact in a release directory (binaries, the Helm
chart, sbom.spdx.json, compatibility-matrix.json) against the sha256
digests recorded in release-manifest.json -- so a downloaded release can
be trusted without re-running the release pipeline (#16, #26).

Does not touch the network. Verifying the container image itself needs a
registry (`docker buildx imagetools inspect <repository>@<digest>`); this
script only prints that instruction rather than attempting it, since
pulling from a registry is a separate concern from verifying the local
artifact bundle.

release-manifest.json's ``sbom`` and ``compatibilityMatrix`` fields were
added in schemaVersion 2; a schemaVersion 1 manifest from an older release
is verified with those two checks skipped rather than failed.

Usage: hack/verify-release.py [release-dir]
"""
import hashlib
import json
import os
import sys


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def check(label, path, want, errors):
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


def main():
    release_dir = sys.argv[1] if len(sys.argv) > 1 else "."
    manifest_path = os.path.join(release_dir, "release-manifest.json")
    if not os.path.isfile(manifest_path):
        print(f"FAIL: release-manifest.json not found in {release_dir}", file=sys.stderr)
        return 1

    with open(manifest_path) as f:
        manifest = json.load(f)

    errors = []
    schema_version = manifest.get("schemaVersion", 0)

    for binary in manifest.get("binaries", []):
        check(f"binary {binary['file']}", os.path.join(release_dir, binary["file"]), binary["sha256"], errors)

    chart = manifest.get("chart")
    if chart:
        check(f"chart {chart['file']}", os.path.join(release_dir, chart["file"]), chart["sha256"], errors)

    sbom = manifest.get("sbom")
    if sbom:
        check(f"sbom {sbom['file']}", os.path.join(release_dir, sbom["file"]), sbom["sha256"], errors)
    elif schema_version < 2:
        print("SKIP: sbom (release-manifest schemaVersion < 2, no sbom digest recorded)")

    compat = manifest.get("compatibilityMatrix")
    if compat:
        check(
            f"compatibilityMatrix {compat['file']}",
            os.path.join(release_dir, compat["file"]),
            compat["sha256"],
            errors,
        )
    elif schema_version < 2:
        print("SKIP: compatibilityMatrix (release-manifest schemaVersion < 2, not recorded)")

    image = manifest.get("image", {})
    if image.get("repository") and image.get("digest"):
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
