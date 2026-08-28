#!/usr/bin/env python3
import argparse, fnmatch, json, sys
from pathlib import Path
RISK_ORDER={"low":0,"medium":1,"high":2}
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
def main():
  p=argparse.ArgumentParser(); p.add_argument("--policy",required=True); p.add_argument("--changed-files",required=True); p.add_argument("--report-out"); a=p.parse_args()
  try:
    policy=load_policy(a.policy); changed=[x.strip() for x in Path(a.changed_files).read_text().splitlines() if x.strip()]
    risk,matches=classify(changed,policy); prefix=policy.get("tracePathPrefix",".agents/evals/traces/")
    trace=any(x.startswith(prefix) and x.endswith(".json") for x in changed); required=risk in set(policy.get("traceRequiredAt",["high"]))
    result={"schemaVersion":"openforge-agent-risk-result/v1","risk":risk,"traceRequired":required,"traceChanged":trace,"changedFiles":changed,"matches":matches}
    if a.report_out: Path(a.report_out).write_text(json.dumps(result,indent=2)+"\n")
    print(json.dumps(result,indent=2))
    if required and not trace:
      print(f"High-risk change requires an operational trace change under {prefix}",file=sys.stderr); return 1
    return 0
  except (OSError,json.JSONDecodeError,ValueError) as e:
    print(f"risk policy error: {e}",file=sys.stderr); return 2
if __name__=="__main__": raise SystemExit(main())
