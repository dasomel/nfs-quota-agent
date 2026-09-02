#!/usr/bin/env bash
# Validates a provenance-meta.json artifact (release.yaml's "Record build
# provenance" step output, part of release-manifest.json's schemaVersion 4
# "provenance" field) BEFORE it is folded into the manifest and signed --
# see the "Generate release manifest" job's --slurpfile provenance merge in
# release.yaml.
#
# Why this exists (independent review of PR #120, #26): the merge step
# folds this file in as `$provenance[0]` with no validation of its own. A
# valid-JSON empty array `[]` produced `provenance: null` in the manifest;
# `{}` produced a v4 manifest whose "provenance" object had none of its
# required fields -- both would have been signed as though they were real
# build-input provenance instead of failing the job outright. This script
# is the single source of truth for what a valid provenance-meta.json looks
# like; the "Record build provenance" step in release.yaml calls it
# directly (rather than duplicating the jq logic there and here), and
# test-check-provenance-meta.sh exercises it against [], {}, and a good
# fixture without needing a real release run.
#
# Usage: check-provenance-meta.sh <path-to-provenance-meta.json>
set -euo pipefail

usage() {
  echo "Usage: $0 <provenance-meta.json>" >&2
  exit 2
}

[ $# -eq 1 ] || usage
file="$1"

if [ ! -f "$file" ]; then
  echo "FAIL: $file not found" >&2
  exit 1
fi

if ! jq empty "$file" >/dev/null 2>&1; then
  echo "FAIL: $file is not valid JSON" >&2
  exit 1
fi

# Every field release-manifest.json's schemaVersion 4 "provenance" object
# requires, checked as non-empty strings (jq's `//` only substitutes on
# null/false, which also covers a plain missing key since `.foo` on an
# object without "foo" evaluates to null).
error=$(jq -r '
  def nonempty: (type == "string") and (length > 0);
  if (type != "object") then
    "must be a JSON object, got " + type
  elif ((.binaryGoVersion // "") | nonempty | not) then "binaryGoVersion missing/empty"
  elif ((.goSum.file // "") | nonempty | not) then "goSum.file missing/empty"
  elif ((.goSum.sha256 // "") | nonempty | not) then "goSum.sha256 missing/empty"
  elif ((.goMod.file // "") | nonempty | not) then "goMod.file missing/empty"
  elif ((.goMod.sha256 // "") | nonempty | not) then "goMod.sha256 missing/empty"
  elif ((.sourceCommit // "") | nonempty | not) then "sourceCommit missing/empty"
  elif ((.sourceTreeHash // "") | nonempty | not) then "sourceTreeHash missing/empty"
  elif ((.builderImage.repository // "") | nonempty | not) then "builderImage.repository missing/empty"
  elif ((.builderImage.digest // "") | nonempty | not) then "builderImage.digest missing/empty"
  elif ((.runtimeImage.repository // "") | nonempty | not) then "runtimeImage.repository missing/empty"
  elif ((.runtimeImage.digest // "") | nonempty | not) then "runtimeImage.digest missing/empty"
  else ""
  end
' "$file")

if [ -n "$error" ]; then
  echo "FAIL: $file is not a valid provenance-meta.json: $error" >&2
  exit 1
fi

echo "OK: $file is a valid provenance-meta.json"
