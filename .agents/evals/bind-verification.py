#!/usr/bin/env python3
"""Bind a real command result into strict verification events without short-circuiting policy evaluation."""
import argparse, json, subprocess, sys, tempfile
from pathlib import Path

ALLOWED = {"verification", "regression_verification"}

def main():
    p = argparse.ArgumentParser()
    p.add_argument("--trace", required=True)
    p.add_argument("--event-id", action="append", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("command", nargs=argparse.REMAINDER)
    a = p.parse_args()
    cmd = a.command[1:] if a.command and a.command[0] == "--" else a.command
    if not cmd:
        print("ERROR: command is required", file=sys.stderr); return 2
    try:
        trace = json.loads(Path(a.trace).read_text(encoding="utf-8"))
    except Exception as e:
        print(f"ERROR: {e}", file=sys.stderr); return 2
    if trace.get("consistencyMode") != "strict":
        print("ERROR: dynamic verification binding requires consistencyMode=strict", file=sys.stderr); return 2
    by_id = {e.get("id"): e for e in trace.get("events", [])}
    targets = []
    for eid in a.event_id:
        event = by_id.get(eid)
        if not event or event.get("type") not in ALLOWED:
            print(f"ERROR: {eid} must identify verification/regression_verification", file=sys.stderr); return 2
        targets.append(event)
    completed = subprocess.run(cmd, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if completed.stdout:
        print(completed.stdout, end="")
    status = "passed" if completed.returncode == 0 else "failed"
    for event in targets:
        event["status"] = status
        event["commandExitCode"] = completed.returncode
        evidence = event.setdefault("evidence", [])
        ref = f"runtime:command-exit-{completed.returncode}"
        if ref not in evidence:
            evidence.append(ref)
    out = Path(a.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(trace, indent=2, ensure_ascii=False) + "\n"
    if out.resolve() == Path(a.trace).resolve():
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=out.parent, delete=False) as f:
            f.write(text); tmp = Path(f.name)
        tmp.replace(out)
    else:
        out.write_text(text, encoding="utf-8")
    print(f"Bound {','.join(a.event_id)} status={status} commandExitCode={completed.returncode}")
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
