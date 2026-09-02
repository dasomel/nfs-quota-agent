#!/usr/bin/env python3
"""Resolve and pin charts/nfs-quota-agent's image.digest (#5).

The chart's templates/_helpers.tpl renders `repository@digest` instead of
`repository:tag` once values.yaml's image.digest is set (see that file's
docstring for why the tag is dropped rather than combined). This script is
the "how do I actually get a digest in there" half of that feature: either
take one directly (--digest, e.g. from a release-manifest.json already on
disk) or resolve one from a live registry reference (--image) via whichever
of crane, skopeo, or `docker buildx imagetools inspect` is on PATH -- the
same three tools hack/verify-release.py's own docstring already points at
for "inspect a remote image without pulling it". It does not duplicate
verify-release.py's job of *verifying* an existing digest against a
manifest; it only resolves and writes one.

Writing into values.yaml is regex-based, not a YAML library round-trip:
this repo's hack/ scripts are stdlib-only (see
validate-compatibility-matrix.py's docstring for the same rule applied to
JSON Schema), and a generic YAML dump would strip every comment in a
values.yaml that is deliberately heavy on them. Only the existing
`digest:` line inside the top-level `image:` mapping is rewritten; every
other line, including that same mapping's own comments, is left untouched
byte-for-byte. Running twice with the same digest is a no-op (idempotent).

Usage:
  hack/update-chart-digest.py --digest sha256:<64 hex> [values.yaml]
  hack/update-chart-digest.py --image ghcr.io/dasomel/nfs-quota-agent:v0.4.1 [values.yaml]
"""
import argparse
import re
import shutil
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_VALUES = REPO_ROOT / "charts" / "nfs-quota-agent" / "values.yaml"

DIGEST_RE = re.compile(r"^sha256:[a-f0-9]{64}$")
DIGEST_SEARCH_RE = re.compile(r"sha256:[0-9a-f]{64}")

# Tools tried in order to resolve an image reference to its digest. Each
# template's stdout/stderr contains a bare or labelled "sha256:<hex>"
# somewhere -- crane prints only the digest, skopeo's --format prints only
# the digest, and `docker buildx imagetools inspect` prints a "Digest:"
# line for the manifest list first -- so one regex search covers all
# three instead of a separate parser per tool.
RESOLVERS = (
    ("crane", ["crane", "digest", "{image}"]),
    ("skopeo", ["skopeo", "inspect", "--format", "{{.Digest}}", "docker://{image}"]),
    ("docker", ["docker", "buildx", "imagetools", "inspect", "{image}"]),
)


def resolve_digest(image_ref):
    """Try each resolver in RESOLVERS, in order, returning (digest, tool)
    from the first one that is on PATH and produces a match. Raises
    RuntimeError naming every tool tried and why it didn't work if none
    do, so the caller gets a concrete "install X or pass --digest"
    message instead of a bare failure."""
    tried = []
    for name, template in RESOLVERS:
        exe = template[0]
        if shutil.which(exe) is None:
            tried.append(f"{name}: not found on PATH")
            continue
        argv = [part.format(image=image_ref) for part in template]
        try:
            result = subprocess.run(argv, capture_output=True, text=True, timeout=60)
        except (OSError, subprocess.TimeoutExpired) as exc:
            tried.append(f"{name}: {exc}")
            continue
        output = result.stdout + result.stderr
        match = DIGEST_SEARCH_RE.search(output)
        if result.returncode == 0 and match:
            return match.group(0), name
        tried.append(f"{name}: exit {result.returncode}" + ("" if match else ", no digest found in output"))
    raise RuntimeError(
        f"could not resolve a digest for {image_ref} -- tried: {'; '.join(tried)}. "
        "Install crane, skopeo, or docker buildx, or pass --digest directly."
    )


def update_values_file(values_path, digest):
    """Rewrite the `digest:` line inside values_path's top-level `image:`
    mapping to `digest: "<digest>"`, preserving indentation, line ending,
    and every other line untouched. Returns True if the file changed,
    False if it already held this exact digest (idempotent no-op).
    Raises ValueError if the file has no image.digest key to update --
    this chart's values.yaml must already declare `image.digest: ""`
    under `image:` (added alongside this script) before a digest can be
    pinned into it."""
    text = values_path.read_text()
    lines = text.splitlines(keepends=True)

    in_image_block = False
    digest_line_idx = None
    for i, line in enumerate(lines):
        stripped = line.rstrip("\n")
        if not in_image_block:
            if re.match(r"^image:\s*$", stripped):
                in_image_block = True
            continue
        if stripped and not stripped[0] in " \t":
            # A new top-level (unindented, non-comment-only) key ends the
            # image: mapping.
            break
        m = re.match(r"^(\s*)digest:", stripped)
        if m:
            digest_line_idx = i
            break

    if digest_line_idx is None:
        raise ValueError(
            f"{values_path} has no 'image.digest' key under 'image:' to update -- "
            "this chart's values.yaml must declare image.digest: \"\" there first "
            "(see charts/nfs-quota-agent/values.yaml)"
        )

    indent = re.match(r"^(\s*)", lines[digest_line_idx]).group(1)
    has_newline = lines[digest_line_idx].endswith("\n")
    new_line = f'{indent}digest: "{digest}"' + ("\n" if has_newline else "")

    if lines[digest_line_idx] == new_line:
        return False

    lines[digest_line_idx] = new_line
    values_path.write_text("".join(lines))
    return True


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument(
        "values_file",
        nargs="?",
        default=str(DEFAULT_VALUES),
        help="path to the chart's values.yaml (default: %(default)s)",
    )
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--digest", help="digest to write directly, e.g. sha256:<64 hex characters>")
    source.add_argument(
        "--image",
        help="image reference to resolve a digest for, e.g. ghcr.io/dasomel/nfs-quota-agent:v0.4.1 "
        "(tried via crane, then skopeo, then docker buildx, in that order)",
    )
    args = parser.parse_args(argv)

    if args.digest:
        digest = args.digest
        source_desc = "--digest"
    else:
        try:
            digest, tool = resolve_digest(args.image)
        except RuntimeError as exc:
            print(f"ERROR: {exc}", file=sys.stderr)
            return 1
        source_desc = f"resolved via {tool} from {args.image}"

    if not DIGEST_RE.match(digest):
        print(
            f"ERROR: {digest!r} ({source_desc}) is not a valid sha256 digest "
            "(expected sha256: followed by 64 lowercase hex characters)",
            file=sys.stderr,
        )
        return 1

    values_path = Path(args.values_file)
    if not values_path.is_file():
        print(f"ERROR: {values_path} not found", file=sys.stderr)
        return 1

    try:
        changed = update_values_file(values_path, digest)
    except ValueError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    if changed:
        print(f"{values_path}: image.digest set to {digest} ({source_desc})")
    else:
        print(f"{values_path}: image.digest already {digest} ({source_desc}), no change")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
