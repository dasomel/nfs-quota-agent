#!/usr/bin/env python3
import argparse,fnmatch,json,sys
from pathlib import Path
P=("test:","ci:","runtime:","artifact:","policy:");PASS={"pass","passed","success","successful","ok","verified"}
def j(p):return json.loads(Path(p).read_text(encoding="utf-8"))
def changed(p):return [x.strip() for x in Path(p).read_text(encoding="utf-8").splitlines() if x.strip()]
def high(paths,policy):return [p for p in paths if "high" in policy.get("traceRequiredAt",[]) and any(r.get("risk")=="high" and fnmatch.fnmatch(p,r["pattern"]) for r in policy.get("rules",[]))]
def covers(t,p):return any(fnmatch.fnmatch(p,g) for g in t.get("changeContext",{}).get("paths",[]))
def validate(t,paths):
 f=[]
 if t.get("schemaVersion")!="openforge-agent-trace/v1":f.append("invalid schemaVersion")
 if t.get("consistencyMode")!="strict":f.append("high-risk trace must use consistencyMode strict")
 if not t.get("changeContext",{}).get("paths"):f.append("changeContext.paths required")
 u=[p for p in paths if not covers(t,p)]
 if u:f.append("uncovered high-risk paths: "+", ".join(u))
 e=[x for x in t.get("events",[]) if x.get("type") in {"verification","regression_verification"}]
 if not e:f.append("verification event required")
 elif not any(str(x.get("scope","")).strip() for x in e):f.append("scoped verification required")
 refs=[r for x in e for r in x.get("evidence",[]) if isinstance(r,str)]
 if not any(r.startswith(P) for r in refs):f.append("typed verification evidence required")
 if not any(str(x.get("status","")).strip().lower() in PASS for x in e):f.append("explicit passed verification required")
 return f
def main():
 a=argparse.ArgumentParser();a.add_argument("--policy",required=True);a.add_argument("--changed-files",required=True);a.add_argument("--trace",action="append",required=True);a.add_argument("--report-out");x=a.parse_args();policy=j(x.policy);hp=high(changed(x.changed_files),policy);tr=[j(p) for p in x.trace];fail=[]
 miss=[p for p in hp if not any(covers(t,p) for t in tr)]
 if miss:fail.append("no trace covers: "+", ".join(miss))
 results=[]
 for src,t in zip(x.trace,tr):
  cov=[p for p in hp if covers(t,p)]
  if not cov:results.append({"trace":src,"traceId":t.get("traceId"),"status":"not-applicable","failures":[]});continue
  tf=validate(t,cov);results.append({"trace":src,"traceId":t.get("traceId"),"status":"evaluated","failures":tf});fail.extend(f"{src}: {v}" for v in tf)
 rep={"schemaVersion":"openforge-agent-evidence-quality/v1","highRiskPaths":hp,"traceResults":results,"passed":not fail,"failures":fail};out=json.dumps(rep,indent=2);print(out)
 if x.report_out:Path(x.report_out).write_text(out+"\n",encoding="utf-8")
 return 1 if fail else 0
if __name__=="__main__":raise SystemExit(main())
