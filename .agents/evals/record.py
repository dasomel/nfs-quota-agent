#!/usr/bin/env python3
import argparse,json
from datetime import datetime,timezone
from pathlib import Path
SCHEMA="openforge-agent-trace/v1"
def main():
 p=argparse.ArgumentParser(); p.add_argument("--trace",required=True); p.add_argument("--trace-id",default="agent-task"); p.add_argument("--task",default="Agent-assisted engineering task"); p.add_argument("--type",required=True,dest="event_type"); p.add_argument("--summary",required=True); p.add_argument("--scope"); p.add_argument("--evidence",action="append",default=[]); p.add_argument("--state",choices=["A","B","C"]); p.add_argument("--next"); p.add_argument("--approved",action="store_true"); p.add_argument("--provenance"); p.add_argument("--reviewed",action="store_true"); a=p.parse_args(); path=Path(a.trace)
 if path.exists():
  data=json.loads(path.read_text());
  if data.get("schemaVersion")!=SCHEMA or not isinstance(data.get("events"),list): raise SystemExit("invalid OpenForge trace")
 else: data={"schemaVersion":SCHEMA,"traceId":a.trace_id,"task":a.task,"createdAt":datetime.now(timezone.utc).isoformat(),"events":[]}
 ids=[int(str(e.get("id","e0"))[1:]) for e in data["events"] if str(e.get("id","")).startswith("e") and str(e.get("id"))[1:].isdigit()]; event={"id":f"e{max(ids,default=0)+1}","type":a.event_type,"summary":a.summary,"recordedAt":datetime.now(timezone.utc).isoformat()}
 for k,v in (("scope",a.scope),("evidence",a.evidence),("state",a.state),("next",a.next),("provenance",a.provenance)):
  if v: event[k]=v
 if a.approved:event["approved"]=True
 if a.reviewed:event["reviewed"]=True
 data["events"].append(event); path.parent.mkdir(parents=True,exist_ok=True); path.write_text(json.dumps(data,indent=2)+"\n"); print(f"recorded {event['id']} {event['type']} -> {path}")
if __name__=="__main__": main()
