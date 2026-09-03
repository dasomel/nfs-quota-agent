#!/usr/bin/env python3
"""Extract a file (default: /licenses/os-packages-manifest.txt) from an OCI image archive.

Given an OCI archive tarball (e.g. produced by `docker buildx build --output type=oci,dest=...`),
this script walks the OCI layout index to find the manifest for a target platform (default: linux/amd64),
and extracts the specified file from the layer tarballs into a destination path.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import tarfile
from pathlib import Path
from typing import Optional


def extract_file_from_oci(
    oci_tar_path: Path,
    dest_path: Path,
    target_os: str = "linux",
    target_arch: str = "amd64",
    target_file: str = "licenses/os-packages-manifest.txt",
) -> None:
    """Extract a file from an OCI tarball for the specified os/architecture."""
    if not oci_tar_path.is_file():
        raise FileNotFoundError(f"OCI archive not found: {oci_tar_path}")

    norm_target = target_file.lstrip("/")

    with tarfile.open(oci_tar_path, mode="r:*") as oci:
        try:
            index_file = oci.extractfile("index.json")
        except KeyError:
            raise ValueError(f"index.json not found in OCI archive {oci_tar_path}")

        if index_file is None:
            raise ValueError(f"Unable to read index.json in {oci_tar_path}")

        index_data = json.load(index_file)
        manifests = index_data.get("manifests") or []
        if not manifests:
            raise ValueError(f"No manifests listed in index.json in {oci_tar_path}")

        top_digest = manifests[0]["digest"]
        top_algo, top_hash = top_digest.split(":", 1)
        top_member = f"blobs/{top_algo}/{top_hash}"

        try:
            top_file = oci.extractfile(top_member)
        except KeyError:
            raise ValueError(f"Blob {top_member} not found in {oci_tar_path}")

        if top_file is None:
            raise ValueError(f"Unable to read blob {top_member} in {oci_tar_path}")

        top_json = json.load(top_file)

        manifest_digest: Optional[str] = None
        if top_json.get("mediaType") == "application/vnd.oci.image.index.v1+json" or "manifests" in top_json:
            for entry in top_json.get("manifests", []):
                p = entry.get("platform") or {}
                if p.get("os") == target_os and p.get("architecture") == target_arch:
                    manifest_digest = entry.get("digest")
                    break
        else:
            manifest_digest = top_digest

        if not manifest_digest:
            raise ValueError(
                f"Manifest for {target_os}/{target_arch} not found in {oci_tar_path}"
            )

        m_algo, m_hash = manifest_digest.split(":", 1)
        m_member = f"blobs/{m_algo}/{m_hash}"
        m_file = oci.extractfile(m_member)
        if m_file is None:
            raise ValueError(f"Unable to read manifest blob {m_member}")

        manifest_json = json.load(m_file)
        layers = manifest_json.get("layers") or []

        found_content: Optional[bytes] = None
        for layer in reversed(layers):
            layer_digest = layer.get("digest", "")
            if not layer_digest or ":" not in layer_digest:
                continue
            l_algo, l_hash = layer_digest.split(":", 1)
            l_member = f"blobs/{l_algo}/{l_hash}"
            try:
                l_file = oci.extractfile(l_member)
            except KeyError:
                continue
            if l_file is None:
                continue

            try:
                with tarfile.open(fileobj=l_file, mode="r:*") as ltar:
                    for member in ltar.getmembers():
                        m_name = member.name.lstrip("./")
                        if m_name == norm_target or m_name.endswith("/" + norm_target):
                            extracted = ltar.extractfile(member)
                            if extracted is not None:
                                found_content = extracted.read()
                                break
            except Exception:
                continue

            if found_content is not None:
                break

        if found_content is None:
            raise ValueError(
                f"Target file {target_file} not found in layers of {target_os}/{target_arch} in {oci_tar_path}"
            )

        dest_path.parent.mkdir(parents=True, exist_ok=True)
        dest_path.write_bytes(found_content)


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        description="Extract a file from an OCI tarball archive."
    )
    parser.add_argument("oci_archive", help="Path to OCI archive (.tar)")
    parser.add_argument("output_path", help="Path to write extracted file to")
    parser.add_argument(
        "--os",
        default="linux",
        help="Target OS (default: linux)",
    )
    parser.add_argument(
        "--arch",
        default="amd64",
        help="Target architecture (default: amd64)",
    )
    parser.add_argument(
        "--file",
        default="licenses/os-packages-manifest.txt",
        help="File inside image to extract",
    )

    args = parser.parse_args(argv)
    try:
        extract_file_from_oci(
            Path(args.oci_archive),
            Path(args.output_path),
            target_os=args.os,
            target_arch=args.arch,
            target_file=args.file,
        )
    except Exception as err:
        print(f"ERROR: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
