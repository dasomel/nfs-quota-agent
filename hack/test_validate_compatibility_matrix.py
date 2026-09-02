#!/usr/bin/env python3
"""Tests for hack/validate-compatibility-matrix.py (#5).

Runs the validator as a subprocess against the real committed matrix and
against mutated copies of it, so these tests exercise the same CLI
contract (argv, exit code, stderr) that `make compat-matrix-validate` and
CI actually invoke -- not just the internal validate() function.
"""
import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HACK_DIR = Path(__file__).resolve().parent
VALIDATOR = HACK_DIR / "validate-compatibility-matrix.py"
SCHEMA = HACK_DIR / "compatibility-matrix.schema.json"
MATRIX = HACK_DIR / "compatibility-matrix.json"


def run_validator(matrix_path, schema_path=SCHEMA):
    return subprocess.run(
        [sys.executable, str(VALIDATOR), "--schema", str(schema_path), str(matrix_path)],
        capture_output=True,
        text=True,
    )


def write_matrix(tmpdir, data):
    path = Path(tmpdir) / "compatibility-matrix.json"
    path.write_text(json.dumps(data))
    return path


def write_schema(tmpdir, data):
    path = Path(tmpdir) / "compatibility-matrix.schema.json"
    path.write_text(json.dumps(data))
    return path


class ValidateCompatibilityMatrixTest(unittest.TestCase):
    def setUp(self):
        self.data = json.loads(MATRIX.read_text())
        self.schema = json.loads(SCHEMA.read_text())
        self._tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmpdir.cleanup)

    def test_committed_matrix_validates(self):
        result = run_validator(MATRIX)
        self.assertEqual(result.returncode, 0, msg=result.stderr)
        self.assertIn("OK", result.stdout)

    def test_bad_status_value_fails_naming_path(self):
        data = copy.deepcopy(self.data)
        data["filesystemBackends"][1]["status"] = "totally-broken"
        matrix_path = write_matrix(self._tmpdir.name, data)

        result = run_validator(matrix_path)

        self.assertEqual(result.returncode, 1)
        self.assertIn("$.filesystemBackends[1].status", result.stderr)

    def test_verified_entry_missing_evidence_fails(self):
        data = copy.deepcopy(self.data)
        # filesystemBackends[0] (xfs) is status "verified" in the committed matrix.
        self.assertEqual(data["filesystemBackends"][0]["status"], "verified")
        del data["filesystemBackends"][0]["evidence"]
        matrix_path = write_matrix(self._tmpdir.name, data)

        result = run_validator(matrix_path)

        self.assertEqual(result.returncode, 1)
        self.assertIn("$.filesystemBackends[0]", result.stderr)
        self.assertIn("evidence", result.stderr)

    def test_unknown_key_fails(self):
        data = copy.deepcopy(self.data)
        data["filesystemBackends"][0]["unexpectedField"] = "surprise"
        matrix_path = write_matrix(self._tmpdir.name, data)

        result = run_validator(matrix_path)

        self.assertEqual(result.returncode, 1)
        self.assertIn("$.filesystemBackends[0]", result.stderr)
        self.assertIn("unexpectedField", result.stderr)

    def test_missing_required_top_level_key_fails(self):
        data = copy.deepcopy(self.data)
        del data["architectures"]
        matrix_path = write_matrix(self._tmpdir.name, data)

        result = run_validator(matrix_path)

        self.assertEqual(result.returncode, 1)
        self.assertIn("$: missing required property 'architectures'", result.stderr)

    def test_unsupported_schema_keyword_is_rejected(self):
        schema = copy.deepcopy(self.schema)
        # "minimum" is not in the validator's implemented keyword set, so it
        # must be rejected outright rather than silently ignored -- ignoring
        # it would let schemaVersion: 1 pass vacuously against "minimum": 999.
        schema["properties"]["schemaVersion"]["minimum"] = 999
        schema_path = write_schema(self._tmpdir.name, schema)

        result = run_validator(MATRIX, schema_path=schema_path)

        self.assertEqual(result.returncode, 1)
        self.assertIn('unsupported schema keyword "minimum"', result.stderr)
        self.assertIn("$.properties.schemaVersion", result.stderr)


if __name__ == "__main__":
    unittest.main()
