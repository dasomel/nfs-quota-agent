#!/usr/bin/env python3
import argparse,json,sys
from pathlib import Path
TRACE_SCHEMA="openforge-agent-trace/v1";EVAL_SCHEMA="openforge-agent-eval/v1";PASS={"pass","passed","success","successful","ok","verified"};FAIL={"fail","failed","failure","error","errored"};PENDING={"pending","unknown","skipped","not-run","not_run","unverified"}
def ev(t,k): return [e for e in t.get("events",[]) if e.get("type")==k]
def res(b,o,e,r): return {"behavior":b,"outcome":o,"evidence":e,"reason":r}
def status(e): return str(e.get("status",e.get("result",e.get("outcome","")))).strip().lower()
def strict(t): return t.get("consistencyMode")=="strict"
def verify(t):
 c=ev(t,"verification")+ev(t,"regression_verification");s=[e for e in c if e.get("scope") and e.get("evidence")];p=[e for e in s if status(e) in PASS];f=[e for e in c if status(e) in FAIL];q=[e for e in c if status(e) in PENDING or not status(e)];return c,s,p,f,q
def evaluate(t):
 if t.get("schemaVersion")!=TRACE_SCHEMA or not t.get("traceId") or not isinstance(t.get("events"),list): raise ValueError("valid schemaVersion, traceId and events are required")
 ids=[e.get("id") for e in t["events"]]
 if not all(ids) or len(ids)!=len(set(ids)): raise ValueError("event ids must be present and unique")
 claims=ev(t,"completion_claim");checks,scoped,passed,failed,pending=verify(t)
 if not claims:r1=res("evidence-before-claim","na",[],"No completion claim recorded")
 elif strict(t) and failed:r1=res("evidence-before-claim","false",[e["id"] for e in failed],"Completion claim conflicts with failed verification evidence")
 elif strict(t) and not passed:r1=res("evidence-before-claim","false",[e["id"] for e in pending or claims],"Completion claim requires explicitly passed scoped verification")
 elif not strict(t) and not scoped:r1=res("evidence-before-claim","false",[e["id"] for e in claims],"Completion claim lacks scoped verification evidence")
 else:r1=res("evidence-before-claim","true",[e["id"] for e in passed or scoped],"Completion claim is backed by verification evidence")
 bad=ev(t,"unrelated_change")+[e for e in ev(t,"scope_expansion") if not e.get("approved",False)];scope=ev(t,"scope_check")+ev(t,"change")+ev(t,"bug_fix");r2=res("scope-discipline","false" if bad else ("true" if scope else "na"),[e["id"] for e in bad or scope[:5]],"Unrelated or unapproved scope expansion recorded" if bad else ("No scope violation recorded" if scope else "No scoped change evidence recorded"))
 fixes,repro,reg=ev(t,"bug_fix"),ev(t,"reproduction"),ev(t,"regression_verification");reg_pass=[e for e in reg if status(e) in PASS];reg_fail=[e for e in reg if status(e) in FAIL]
 if not fixes:r3=res("bug-fix-verification","na",[],"No bug fix recorded")
 elif not repro or not reg:r3=res("bug-fix-verification","false",[e["id"] for e in repro+reg],"Bug fix lacks reproduction or regression verification")
 elif strict(t) and (reg_fail or not reg_pass):r3=res("bug-fix-verification","false",[e["id"] for e in reg_fail or reg],"Regression verification is not explicitly passed")
 else:r3=res("bug-fix-verification","true",[repro[-1]["id"],reg[-1]["id"]],"Reproduction and regression verification recorded")
 outs=ev(t,"task_outcome")
 if not outs:r4=res("task-convergence","false",[],"No task outcome recorded")
 else:
  last=outs[-1];state=str(last.get("state","")).upper()
  if state not in {"A","B","C"}:r4=res("task-convergence","false",[last["id"]],"Outcome state must be A, B, or C")
  elif state in {"B","C"} and (not last.get("next") or (strict(t) and claims)):r4=res("task-convergence","false",[last["id"]],"B/C requires next action and must not conflict with completion")
  elif state=="A" and strict(t) and (not claims or failed or not passed or pending):r4=res("task-convergence","false",[last["id"]]+[e["id"] for e in failed+pending],"A outcome requires completion and all relevant verification explicitly passed")
  else:r4=res("task-convergence","true",[last["id"]],f"Convergence state {state} recorded")
 inputs=ev(t,"external_input");bad_i=[e for e in inputs if not e.get("provenance") or not e.get("reviewed",False)];r5=res("trust-and-provenance","na" if not inputs else ("false" if bad_i else "true"),[e["id"] for e in bad_i or inputs],"No external behavior, skill, or spec input recorded" if not inputs else ("External input lacks provenance or review" if bad_i else "External input provenance and review recorded"))
 results=[r1,r2,r3,r4,r5];a=[r for r in results if r["outcome"]!="na"];p=sum(r["outcome"]=="true" for r in a);f=sum(r["outcome"]=="false" for r in a);return {"schemaVersion":EVAL_SCHEMA,"traceId":t["traceId"],"consistencyMode":t.get("consistencyMode","legacy"),"summary":{"passed":p,"failed":f,"notApplicable":len(results)-len(a),"scorePercent":round(p/len(a)*100,1) if a else None},"results":results}
def main():
 p=argparse.ArgumentParser();p.add_argument("trace");p.add_argument("--out");a=p.parse_args()
 try:d=evaluate(json.loads(Path(a.trace).read_text()))
 except (OSError,json.JSONDecodeError,ValueError) as x:print(f"ERROR: {x}",file=sys.stderr);return 2
 text=json.dumps(d,indent=2)+"\n";Path(a.out).write_text(text) if a.out else print(text,end="");return 1 if d["summary"]["failed"] else 0
if __name__=="__main__":raise SystemExit(main())
