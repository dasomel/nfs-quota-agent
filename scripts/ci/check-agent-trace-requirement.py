#!/usr/bin/env python3
import argparse, fnmatch, json, re, subprocess, sys
from pathlib import Path
RISK_ORDER={"low":0,"medium":1,"high":2}
PIN_LINE_RE = re.compile(
    r'^(?P<indent>\s*)uses:\s+(?P<action>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*)@'
    r'(?P<sha>[0-9a-f]{40})\s+#\s+(?P<version>v\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)\s*$'
)
# Matches a Dockerfile `FROM <image>@sha256:<digest>` line, with the optional
# `--platform=...` prefix and `AS <stage>` suffix this repo's Dockerfile
# uses. The repository part before `@sha256:` is captured whole (not split
# into name/tag) so a tag change (e.g. golang:1.26-alpine -> 1.27-alpine)
# is visible to the pin-only comparison below rather than silently ignored.
DOCKERFILE_FROM_RE = re.compile(
    r'^(?P<prefix>FROM\s+(?:--platform=\S+\s+)?)(?P<image>\S+)@sha256:'
    r'(?P<digest>[0-9a-f]{64})(?P<suffix>\s+[Aa][Ss]\s+\S+)?\s*$'
)
def load_policy(path):
  data=json.loads(Path(path).read_text())
  if data.get("schemaVersion")!="openforge-agent-risk-policy/v1" or not isinstance(data.get("rules"),list): raise ValueError("invalid risk policy")
  return data
def classify(paths,policy):
  highest=policy.get("defaultRisk","low"); matches=[]
  if highest not in RISK_ORDER: raise ValueError("unknown default risk")
  for path in paths:
    for rule in policy["rules"]:
      risk=rule.get("risk"); pattern=rule.get("pattern")
      if risk not in RISK_ORDER or not pattern: raise ValueError("invalid rule")
      if fnmatch.fnmatchcase(path,pattern):
        matches.append({"path":path,"risk":risk,"pattern":pattern,"reason":rule.get("reason","")})
        if RISK_ORDER[risk]>RISK_ORDER[highest]: highest=risk
  return highest,matches
def _parse_pin_hunks(diff_text):
  """Parse a unified diff (context=0) into a list of hunks, each a list of
  (marker, line) tuples with the diff marker stripped from the payload."""
  hunks=[]; current=None
  for line in diff_text.splitlines():
    if line.startswith("@@"):
      current=[]; hunks.append(current); continue
    if current is None:
      continue
    if line.startswith("+++") or line.startswith("---"):
      continue
    if line.startswith("+") or line.startswith("-"):
      current.append((line[0], line[1:]))
  return hunks
def is_dependabot_action_pin_only(changed_files, base_sha, head_sha, github_actor, pr_author):
  if github_actor != "dependabot[bot]" or pr_author != "dependabot[bot]":
    return False
  if not changed_files:
    return False
  for path in changed_files:
    if not (fnmatch.fnmatchcase(path, ".github/workflows/*.yml") or fnmatch.fnmatchcase(path, ".github/workflows/*.yaml")):
      return False
  if not base_sha or not head_sha:
    return False
  try:
    proc = subprocess.run(
      ["git", "diff", "--unified=0", base_sha, head_sha, "--", ".github/workflows"],
      capture_output=True, text=True, check=True,
    )
  except (OSError, subprocess.CalledProcessError):
    return False
  hunks = _parse_pin_hunks(proc.stdout)
  if not hunks:
    return False
  for hunk in hunks:
    if len(hunk) != 2:
      return False
    (marker_a, line_a), (marker_b, line_b) = hunk
    if marker_a != "-" or marker_b != "+":
      return False
    match_old = PIN_LINE_RE.match(line_a)
    match_new = PIN_LINE_RE.match(line_b)
    if not match_old or not match_new:
      return False
    if match_old.group("indent") != match_new.group("indent") or match_old.group("action") != match_new.group("action"):
      return False
    if match_old.group("sha") == match_new.group("sha") or match_old.group("version") == match_new.group("version"):
      return False
  return True
def is_dependabot_dockerfile_pin_only(changed_files, base_sha, head_sha, github_actor, pr_author):
  """Exempts a Dependabot-authored PR whose only change to Dockerfile* is
  the image reference (tag and/or digest) on a `FROM ...@sha256:...` line.
  Tag changes are allowed here, not just digest changes, because the
  ignore rules in .github/dependabot.yml already keep Dependabot from
  proposing a golang/alpine minor or major bump on these images -- so a
  tag change this function ever sees is a same-minor patch refresh (e.g.
  1.26.7-alpine -> 1.26.8-alpine, or 3.24.0 -> 3.24.1), which is exactly
  the kind of digest-freshness bump this exemption exists for. A RUN line,
  apk package list change, or anything else in Dockerfile* still fails
  the strict single-hunk-per-change check below and stays non-exempt.
  """
  if github_actor != "dependabot[bot]" or pr_author != "dependabot[bot]":
    return False
  if not changed_files:
    return False
  for path in changed_files:
    if not fnmatch.fnmatchcase(path, "Dockerfile*"):
      return False
  if not base_sha or not head_sha:
    return False
  try:
    proc = subprocess.run(
      ["git", "diff", "--unified=0", base_sha, head_sha, "--", "Dockerfile*"],
      capture_output=True, text=True, check=True,
    )
  except (OSError, subprocess.CalledProcessError):
    return False
  hunks = _parse_pin_hunks(proc.stdout)
  if not hunks:
    return False
  for hunk in hunks:
    if len(hunk) != 2:
      return False
    (marker_a, line_a), (marker_b, line_b) = hunk
    if marker_a != "-" or marker_b != "+":
      return False
    match_old = DOCKERFILE_FROM_RE.match(line_a)
    match_new = DOCKERFILE_FROM_RE.match(line_b)
    if not match_old or not match_new:
      return False
    if match_old.group("prefix") != match_new.group("prefix") or match_old.group("suffix") != match_new.group("suffix"):
      return False
    if match_old.group("image") == match_new.group("image") and match_old.group("digest") == match_new.group("digest"):
      return False
  return True
def main():
  p=argparse.ArgumentParser()
  p.add_argument("--policy",required=True)
  p.add_argument("--changed-files",required=True)
  p.add_argument("--report-out")
  p.add_argument("--base-sha",default=None)
  p.add_argument("--head-sha",default=None)
  p.add_argument("--github-actor",default=None)
  p.add_argument("--pr-author",default=None)
  a=p.parse_args()
  try:
    policy=load_policy(a.policy); changed=[x.strip() for x in Path(a.changed_files).read_text().splitlines() if x.strip()]
    risk,matches=classify(changed,policy); prefix=policy.get("tracePathPrefix",".agents/evals/traces/")
    trace=any(x.startswith(prefix) and x.endswith(".json") for x in changed); required=risk in set(policy.get("traceRequiredAt",["high"]))
    trace_exempt=False; trace_exempt_reason=None
    if required and not trace:
      if is_dependabot_action_pin_only(changed, a.base_sha, a.head_sha, a.github_actor, a.pr_author):
        trace_exempt=True
        trace_exempt_reason="dependabot pin-only bump of GitHub Actions SHA(s) in workflow file(s)"
      elif is_dependabot_dockerfile_pin_only(changed, a.base_sha, a.head_sha, a.github_actor, a.pr_author):
        trace_exempt=True
        trace_exempt_reason="dependabot pin-only bump of Dockerfile FROM image reference(s)"
    result={"schemaVersion":"openforge-agent-risk-result/v1","risk":risk,"traceRequired":required,"traceChanged":trace,"traceExempt":trace_exempt,"traceExemptReason":trace_exempt_reason,"changedFiles":changed,"matches":matches}
    if a.report_out: Path(a.report_out).write_text(json.dumps(result,indent=2)+"\n")
    print(json.dumps(result,indent=2))
    if required and not trace and not trace_exempt:
      print(f"High-risk change requires an operational trace change under {prefix}",file=sys.stderr); return 1
    return 0
  except (OSError,json.JSONDecodeError,ValueError) as e:
    print(f"risk policy error: {e}",file=sys.stderr); return 2
if __name__=="__main__": raise SystemExit(main())
