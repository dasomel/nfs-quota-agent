#!/usr/bin/env python3
"""Regression tests for the Makefile's release-bundle target and
hack/BUNDLE-README.md.tmpl (#5).

Covers two rounds of external review on PR #117:

- An opus re-review's MEDIUM: the Makefile must copy
  hack/sigstore-trusted-root.json into the bundle (for reference), and
  hack/BUNDLE-README.md.tmpl must mention it.
- A later Codex critic pass's CRITICAL: the README must NOT tell a user to
  run the verifier or trust the trust root FROM INSIDE the bundle to
  verify that same bundle -- whoever can replace a bundle can replace the
  verifier/trust root packaged alongside it. The documented procedure must
  fetch both out-of-band (a separate git checkout of the signed tag, or
  the release's own separately-signed assets) and run them against the
  still-unextracted archive.

Doesn't invoke `make release-bundle` itself (that needs skopeo, docker,
and an already-built image/chart, none of which this stdlib-only test
should require) -- instead greps the actual Makefile recipe text and the
README template, which is exactly what would regress if a future edit
reordered/dropped a line or watered down the warning.
"""
import os
import unittest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.realpath(__file__)))
MAKEFILE = os.path.join(REPO_ROOT, "Makefile")
README_TMPL = os.path.join(REPO_ROOT, "hack", "BUNDLE-README.md.tmpl")


def release_bundle_recipe():
    """Extracts the release-bundle target's recipe lines (from the
    "release-bundle:" line up to the next non-indented/blank line)."""
    with open(MAKEFILE) as f:
        lines = f.readlines()
    start = None
    for i, line in enumerate(lines):
        if line.startswith("release-bundle:"):
            start = i
            break
    assert start is not None, "release-bundle: target not found in Makefile"
    recipe = []
    for line in lines[start + 1:]:
        if line.strip() == "" or line.startswith("\t"):
            recipe.append(line)
            if line.strip() == "":
                break
        else:
            break
    return "".join(recipe)


class ReleaseBundleMakefileTest(unittest.TestCase):
    def setUp(self):
        self.recipe = release_bundle_recipe()
        with open(README_TMPL) as f:
            self.readme = f.read()

    def test_copies_verify_release_script(self):
        self.assertIn("hack/verify-release.py", self.recipe)

    def test_copies_sigstore_trusted_root(self):
        self.assertIn("hack/sigstore-trusted-root.json", self.recipe)
        self.assertIn('$(BUNDLE_STAGE)/hack/sigstore-trusted-root.json', self.recipe)

    def test_sigstore_trusted_root_file_exists_in_repo(self):
        """Sanity: the file the Makefile copies actually exists, so the cp
        in a real run doesn't fail."""
        self.assertTrue(
            os.path.isfile(os.path.join(REPO_ROOT, "hack", "sigstore-trusted-root.json"))
        )

    def test_require_signed_manifest_guard_present(self):
        """HIGH (Codex critic pass on #117): release.yaml's release-bundle
        job must fail loudly, not silently produce an unverifiable bundle,
        when release-manifest.json or its cosign signature bundle is
        missing from the downloaded release assets."""
        self.assertIn("REQUIRE_SIGNED_MANIFEST", self.recipe)
        self.assertIn("release-manifest.json.bundle", self.recipe)

    def test_copies_sbom_when_present(self):
        self.assertIn("sbom.spdx.json", self.recipe)

    def test_readme_warns_against_in_archive_verification(self):
        """CRITICAL (Codex critic pass on #117): the README must not
        instruct running the bundled verifier/trust root against the
        bundle they ship inside -- that lets a forged bundle carry a
        forged verifier that always prints OK."""
        self.assertIn("Do not run", self.readme)
        self.assertIn("out-of-band", self.readme)
        self.assertIn("git clone", self.readme)

    def test_readme_require_signatures_command_uses_out_of_band_trusted_root(self):
        """The --require-signatures example must point --trusted-root at
        the out-of-band checkout, not rely on the default (which would
        resolve next to whichever verify-release.py was actually run --
        including, dangerously, one extracted from inside this archive)."""
        self.assertIn("--require-signatures", self.readme)
        self.assertIn("--trusted-root", self.readme)

    def test_readme_lists_external_assets_to_fetch(self):
        """MEDIUM (Codex final verification on #117): a user who only
        downloads the bundle itself has no way to run --require-signatures
        successfully -- the README must name every external asset needed
        (the bundle's own .sha256/.bundle, and crucially
        release-manifest.json + release-manifest.json.bundle) rather than
        only mentioning the bundle and the out-of-band verifier."""
        for asset in (
            "release-manifest.json",
            "release-manifest.json.bundle",
            "-offline.tar.gz.sha256",
            "-offline.tar.gz.bundle",
        ):
            self.assertIn(asset, self.readme, f"README does not mention fetching {asset}")

    def test_readme_verify_command_passes_manifest_flag(self):
        """The baseline (non---require-signatures) verify command shown in
        the README must itself pass --manifest -- verify-release.py now
        requires it (see hack/verify-release.py's main()), so an example
        without it would not even run."""
        self.assertIn("--manifest release-manifest.json", self.readme)

    def test_readme_documents_decision_d_auto_discovery(self):
        """Decision D (Codex delta re-check on #117): auto-discovery of a
        sibling release-manifest.json is kept (it's not a trust hole -- it
        goes through the same cosign check as an explicit --manifest), but
        the README must say so explicitly and still recommend the explicit
        --manifest form."""
        self.assertIn("Decision D", self.readme)
        self.assertIn("auto-discover", self.readme.lower())
        self.assertIn("recommended", self.readme.lower())

    def test_verify_release_requires_manifest_flag_for_bundle_mode(self):
        """Pins the actual enforcement this README behavior depends on:
        hack/verify-release.py's main() must require --manifest (or an
        auto-discovered release-manifest.json next to the bundle) for
        --bundle mode, not just document that it should be passed."""
        verify_release_path = os.path.join(REPO_ROOT, "hack", "verify-release.py")
        with open(verify_release_path) as f:
            verify_release_src = f.read()
        self.assertIn("no release-manifest.json found", verify_release_src)


if __name__ == "__main__":
    unittest.main()
