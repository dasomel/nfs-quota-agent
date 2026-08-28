#!/usr/bin/env python3
import argparse, json, sys
from pathlib import Path
TRACE_SCHEMA="openforge-agent-trace/v1"; EVAL_SCHEMA="openforge-agent-eval/v1"
def ev(t,k): return [e for e in t.get("events",[]) if e.get("type")==k]
def res(b,o,e,r): return {"behavior":b,"outcome":o,"evidence":e,"reason":r}
def evaluate(t):
    if t.get("schemaVersion")!=TRACE_SCHEMA: raise ValueError(f"schemaVersion must be {TRACE_SCHEMA}")
    if not t.get("traceId") or not isinstance(t.get("events"),list): raise ValueError("traceId and events are required")
    ids=[e.get("id") for e in t["events"]]
    if not all(ids) or len(ids)!=len(set(ids)): raise ValueError("event ids must be present and unique")
    claims=ev(t,"completion_claim"); checks=[e for e in ev(t,"verification") if e.get("scope") and e.get("evidence")]
    r1=res("evidence-before-claim","na" if not claims else ("true" if checks else "false"),[e["id"] for e in checks or claims],"Scoped verification evidence recorded" if checks else ("No completion claim recorded" if not claims else "Completion claim lacks scoped verification evidence"))
    bad=ev(t,"unrelated_change")+[e for e in ev(t,"scope_expansion") if not e.get("approved",False)]; scoped=ev(t,"scope_check")+ev(t,"change")+ev(t,"bug_fix")
    r2=res("scope-discipline","false" if bad else ("true" if scoped else "na"),[e["id"] for e in bad or scoped[:5]],"Unrelated or unapproved scope expansion recorded" if bad else ("No scope violation recorded" if scoped else "No scoped change evidence recorded"))
    fixes,repro,reg=ev(t,"bug_fix"),ev(t,"reproduction"),ev(t,"regression_verification")
    r3=res("bug-fix-verification","na" if not fixes else ("true" if repro and reg else "false"),[e["id"] for e in repro[-1:]+reg[-1:]],"No bug fix recorded" if not fixes else ("Reproduction and regression verification recorded" if repro and reg else "Bug fix lacks reproduction or regression verification"))
    out=ev(t,"task_outcome")
    if not out: r4=res("task-convergence","false",[],"No task outcome recorded")
    else:
        last=out[-1]; state=str(last.get("state","")).upper(); ok=state in {"A","B","C"} and (state=="A" or bool(last.get("next")))
        r4=res("task-convergence","true" if ok else "false",[last["id"]],f"Convergence state {state} recorded" if ok else "Invalid or incomplete convergence outcome")
    inputs=ev(t,"external_input"); bad_i=[e for e in inputs if not e.get("provenance") or not e.get("reviewed",False)]
    r5=res("trust-and-provenance","na" if not inputs else ("false" if bad_i else "true"),[e["id"] for e in bad_i or inputs],"No external behavior, skill, or spec input recorded" if not inputs else ("External input lacks provenance or review" if bad_i else "External input provenance and review recorded"))
    results=[r1,r2,r3,r4,r5]; applicable=[r for r in results if r["outcome"]!="na"]; passed=sum(r["outcome"]=="true" for r in applicable); failed=sum(r["outcome"]=="false" for r in applicable)
    return {"schemaVersion":EVAL_SCHEMA,"traceId":t["traceId"],"summary":{"passed":passed,"failed":failed,"notApplicable":len(results)-len(applicable),"scorePercent":round(passed/len(applicable)*100,1) if applicable else None},"results":results}
def main():
    p=argparse.ArgumentParser(); p.add_argument("trace"); p.add_argument("--out"); a=p.parse_args()
    try: data=evaluate(json.loads(Path(a.trace).read_text()))
    except (OSError,json.JSONDecodeError,ValueError) as x: print(f"ERROR: {x}",file=sys.stderr); return 2
    text=json.dumps(data,indent=2)+"\n"; Path(a.out).write_text(text) if a.out else print(text,end=""); return 1 if data["summary"]["failed"] else 0
if __name__=="__main__": raise SystemExit(main())
