#!/usr/bin/env python3
"""Validate hack/compatibility-matrix.json against its JSON Schema (#5).

Deliberately does not depend on the third-party `jsonschema` package --
this repo's hack/ scripts are stdlib-only so an air-gapped build never
needs to `pip install` anything to run them. Instead this implements the
small subset of JSON Schema (draft 2020-12) that
hack/compatibility-matrix.schema.json actually uses:

    type, required, properties, additionalProperties, items, enum,
    minLength, and if/then (used only for the status -> evidence
    conditional: when a "status" property is "verified", "evidence" must
    be a non-empty string).

Anything outside that keyword set (patternProperties, oneOf/anyOf/allOf,
$ref, format, numeric bounds, ...) is silently ignored rather than
enforced. That is fine for this one schema, which was hand-written to
stay inside the subset -- it is not a general-purpose validator.

Usage: hack/validate-compatibility-matrix.py [--schema PATH] [matrix.json]
Exit 0 with a one-line summary on success; exit 1 listing every
violation (one per line, prefixed with its JSON path) on failure.
"""
import argparse
import json
import sys
from pathlib import Path

DEFAULT_SCHEMA = Path(__file__).resolve().parent / "compatibility-matrix.schema.json"
DEFAULT_MATRIX = Path(__file__).resolve().parent / "compatibility-matrix.json"


def _type_ok(instance, expected):
    if expected == "object":
        return isinstance(instance, dict)
    if expected == "array":
        return isinstance(instance, list)
    if expected == "string":
        return isinstance(instance, str)
    if expected == "integer":
        return isinstance(instance, int) and not isinstance(instance, bool)
    if expected == "number":
        return isinstance(instance, (int, float)) and not isinstance(instance, bool)
    if expected == "boolean":
        return isinstance(instance, bool)
    if expected == "null":
        return instance is None
    return True


def validate(instance, schema, path, errors):
    """Recursively check `instance` against `schema`, appending
    "<path>: <message>" strings to `errors` for every violation found."""
    if "type" in schema and not _type_ok(instance, schema["type"]):
        errors.append(f"{path}: expected type '{schema['type']}', got {type(instance).__name__}")
        return

    if "enum" in schema and instance not in schema["enum"]:
        errors.append(f"{path}: {instance!r} is not one of {schema['enum']}")

    if "minLength" in schema and isinstance(instance, str) and len(instance) < schema["minLength"]:
        errors.append(f"{path}: length {len(instance)} is shorter than minLength {schema['minLength']}")

    if isinstance(instance, dict):
        for req in schema.get("required", []):
            if req not in instance:
                errors.append(f"{path}: missing required property '{req}'")
        props = schema.get("properties", {})
        if schema.get("additionalProperties") is False:
            for key in instance:
                if key not in props:
                    errors.append(f"{path}: additional property '{key}' is not allowed")
        for key, subschema in props.items():
            if key in instance:
                validate(instance[key], subschema, f"{path}.{key}", errors)

    if isinstance(instance, list) and "items" in schema:
        for i, item in enumerate(instance):
            validate(item, schema["items"], f"{path}[{i}]", errors)

    if "if" in schema:
        if_errors = []
        validate(instance, schema["if"], path, if_errors)
        if not if_errors and "then" in schema:
            validate(instance, schema["then"], path, errors)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("matrix", nargs="?", default=str(DEFAULT_MATRIX), help="path to compatibility-matrix.json")
    parser.add_argument("--schema", default=str(DEFAULT_SCHEMA), help="path to the JSON Schema to validate against")
    args = parser.parse_args(argv)

    try:
        schema = json.loads(Path(args.schema).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"ERROR: could not read schema {args.schema}: {exc}", file=sys.stderr)
        return 1

    try:
        instance = json.loads(Path(args.matrix).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"ERROR: could not read {args.matrix}: {exc}", file=sys.stderr)
        return 1

    errors = []
    validate(instance, schema, "$", errors)

    if errors:
        print(f"{args.matrix} failed schema validation ({len(errors)} violation(s)):", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        return 1

    sections = ["filesystemBackends", "architectures", "kubernetesVersions", "knownLimitations"]
    total = sum(len(instance.get(s, [])) for s in sections)
    print(f"{args.matrix} OK ({total} entries across {len(sections)} sections, schema {args.schema})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
