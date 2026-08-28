#!/usr/bin/env python3
import argparse,fnmatch,json,sys
from pathlib import Path
P=("test:","ci:","runtime:","artifact:","policy:")
def j(p): return json.loads(Path(p).read_text(encoding="utf-8"))
def changed(p): return [x.strip() for x in Path(p).read_text(encoding="utf-8").splitlines() if x.strip()]
def high(paths,policy): return [p for p in paths if "high" in policy.get("traceRequiredAt",[]) and any(r.get("risk")=="high" and fnmatch.fnmatch(p,r["pattern"]) for r in policy.get("rules",[]))]
def covers(t,p): return any(fnmatch.fnmatch(p,g) for g in t.get("changeContext",{}).get("paths",[]))
def validate(t,paths):
  f=[]
  if t.get("schemaVersion")!="openforge-agent-trace/v1": f.append("invalid schemaVersion")
  if not t.get("changeContext",{}).get("paths"): f.append("changeContext.paths required")
  u=[p for p in paths if not covers(t,p)]
  if u: f.append("uncovered high-risk paths: "+", ".join(u))
  ev=[e for e in t.get("events",[]) if e.get("type") in {"verification","regression_verification"}]
  if not ev: f.append("verification event required")
  elif not any(str(e.get("scope","")).strip() for e in ev): f.append("scoped verification required")
  refs=[r for e in ev for r in e.get("evidence",[]) if isinstance(r,str)]
  if not any(r.startswith(P) for r in refs): f.append("typed verification evidence required")
  return f
def main():
  a=argparse.ArgumentParser();a.add_argument("--policy",required=True);a.add_argument("--changed-files",required=True);a.add_argument("--trace",action="append",required=True);a.add_argument("--report-out");x=a.parse_args()
  policy=j(x.policy); paths=changed(x.changed_files); hp=high(paths,policy); traces=[j(p) for p in x.trace]; failures=[]
  missing=[p for p in hp if not any(covers(t,p) for t in traces)]
  if missing: failures.append("no trace covers: "+", ".join(missing))
  results=[]
  for src,t in zip(x.trace,traces):
    tf=validate(t,[p for p in hp if covers(t,p)]); results.append({"trace":src,"traceId":t.get("traceId"),"failures":tf}); failures.extend(f"{src}: {v}" for v in tf)
  report={"schemaVersion":"openforge-agent-evidence-quality/v1","highRiskPaths":hp,"traceResults":results,"passed":not failures,"failures":failures}; out=json.dumps(report,indent=2); print(out)
  if x.report_out: Path(x.report_out).write_text(out+"\n",encoding="utf-8")
  return 1 if failures else 0
if __name__=="__main__": raise SystemExit(main())
