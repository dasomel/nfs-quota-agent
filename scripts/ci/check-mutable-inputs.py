#!/usr/bin/env python3
import argparse,re,sys
from pathlib import Path
ACTION_RE=re.compile(r"^\s*uses:\s*([^\s#]+)")
SHA40_RE=re.compile(r"^[0-9a-f]{40}$")
PATTERNS=(("container latest tag",re.compile(r"(?<![\w.-]):latest\b")),("latest release download",re.compile(r"/releases/latest(?:/download)?(?:/|\b)")))
def files(paths):
  for raw in paths:
    p=Path(raw)
    if not p.exists(): raise FileNotFoundError(raw)
    if p.is_file(): yield p
    else:
      for c in sorted(p.rglob('*')):
        if c.is_file() and '.git' not in c.parts: yield c
def scan(p):
  out=[]
  try: lines=p.read_text(encoding='utf-8').splitlines()
  except UnicodeDecodeError: return out
  for n,line in enumerate(lines,1):
    s=line.strip()
    if not s or s.startswith('#'): continue
    for label,pattern in PATTERNS:
      if pattern.search(line): out.append((n,label,s))
    m=ACTION_RE.match(line)
    if m:
      ref=m.group(1)
      if ref.startswith('./'): continue
      if '@' not in ref or not SHA40_RE.fullmatch(ref.rsplit('@',1)[1]): out.append((n,'GitHub Action ref is not a 40-char commit SHA',s))
  return out
def main():
  ap=argparse.ArgumentParser(); ap.add_argument('paths',nargs='+'); args=ap.parse_args(); bad=[]
  try: protected=list(files(args.paths))
  except FileNotFoundError as e:
    print(f'ERROR: protected path does not exist: {e}',file=sys.stderr); return 2
  for p in protected:
    for n,label,text in scan(p): bad.append((p,n,label,text))
  if bad:
    for p,n,label,text in bad: print(f'{p}:{n}: {label}: {text}',file=sys.stderr)
    return 1
  print(f'Mutable-input guard passed for {len(protected)} file(s)'); return 0
if __name__=='__main__': raise SystemExit(main())
