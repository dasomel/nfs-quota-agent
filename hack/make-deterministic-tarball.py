#!/usr/bin/env python3
"""Write a reproducible .tar or .tar.gz of a staging directory (#5 offline
bundle).

`tar` itself is not a safe cross-platform choice for this: GNU tar's
--sort/--mtime/--owner/--numeric-owner flags either don't exist on macOS's
bundled bsdtar (libarchive) or behave differently, so the archive digest
would depend on which `tar` built it -- exactly the property a
reproducibility check (`make release-bundle` twice, diff the sha256) is
supposed to rule out. Building the archive directly with Python's stdlib
`tarfile` sidesteps that: every entry's mtime, uid/gid, uname/gname, and
mode is normalized here regardless of platform, and entries are added in
a fixed sorted order.

Output format is chosen by output_path's extension: ``.tar.gz``/``.tgz``
gzips (with the gzip header's own mtime pinned too, otherwise two runs
differ at the byte level even with identical tar content); anything else
(e.g. ``.tar``) is written uncompressed -- used for the OCI image archive
inside a release bundle, since `skopeo copy ... oci-archive:` and `docker
buildx --output type=oci` both emit tar headers with the current
wall-clock time, which this script re-normalizes by unpacking and
rebuilding the archive through the same deterministic path as everything
else in the bundle (see Makefile's release-bundle target).

Only regular files are walked and added (``os.walk`` + one ``tf.add(...,
recursive=False)`` per file) -- directory entries and symlinks in the
staging tree are NOT preserved as their own tar members; a directory that
contains no files is silently dropped, and a symlink is neither followed
nor archived as a link. This is deliberate for this script's one real
caller (an OCI archive re-normalization and the top-level bundle
directory, both of which are plain files under known directory names) but
means it is not a general-purpose reproducible-tar tool: a staging tree
that depends on an empty directory existing after extraction, or on a
symlink being preserved as a link rather than silently omitted, will not
round-trip through this script.

Usage: hack/make-deterministic-tarball.py <staging-dir> <output-path> [--mtime SECONDS]
"""
import argparse
import gzip
import os
import sys
import tarfile


def build(staging_dir, output_path, mtime):
    # Sort so the member order -- not just each member's own metadata -- is
    # identical across runs and across machines with different directory
    # walk orders (APFS vs ext4 readdir order is not guaranteed stable).
    members = []
    for root, dirs, files in os.walk(staging_dir):
        dirs.sort()
        for name in sorted(files):
            full = os.path.join(root, name)
            arcname = os.path.relpath(full, staging_dir)
            members.append((full, arcname))
    members.sort(key=lambda pair: pair[1])

    def normalize(tarinfo):
        tarinfo.mtime = mtime
        tarinfo.uid = 0
        tarinfo.gid = 0
        tarinfo.uname = ""
        tarinfo.gname = ""
        # Regular files/dirs only in this bundle; keep whatever mode the
        # staged file already has (e.g. verify-release.py stays executable)
        # rather than forcing one, since that mode is itself content, not
        # incidental metadata.
        return tarinfo

    gzip_output = output_path.endswith((".tar.gz", ".tgz"))
    if gzip_output:
        # filename="" is required, not cosmetic: GzipFile defaults to
        # fileobj.name (the output path) as the embedded original-filename
        # header field (FNAME flag) when none is given explicitly, which
        # makes the compressed byte stream depend on the output path's
        # name -- e.g. "out1.tar.gz" vs "out2.tar.gz" over identical
        # content previously produced two different sha256 digests.
        with open(output_path, "wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", mtime=mtime, filename="") as gz:
            with tarfile.open(fileobj=gz, mode="w") as tf:
                for full, arcname in members:
                    tf.add(full, arcname=arcname, recursive=False, filter=normalize)
    else:
        with tarfile.open(output_path, mode="w") as tf:
            for full, arcname in members:
                tf.add(full, arcname=arcname, recursive=False, filter=normalize)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("staging_dir")
    parser.add_argument("output_path")
    parser.add_argument("--mtime", type=int, default=0, help="Unix timestamp written into every archive entry (default: 0, i.e. SOURCE_DATE_EPOCH-style pinning)")
    args = parser.parse_args()
    if not os.path.isdir(args.staging_dir):
        print(f"FAIL: {args.staging_dir} is not a directory", file=sys.stderr)
        return 1
    build(args.staging_dir, args.output_path, args.mtime)
    return 0


if __name__ == "__main__":
    sys.exit(main())
