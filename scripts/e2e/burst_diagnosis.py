"""Best-effort, read-only burst evidence; no cluster access on import."""

import datetime
import json
from pathlib import Path
import re
import shlex
import time


NEEDED = (
  'Per correlation_id/PV: enqueue, worker-start, mutex-acquired, '
  'command start/end (including verify), annotation GET/UPDATE start/end with '
  'limiter-wait duration, audit/event end; startup effective REST QPS/burst. '
  'Existing backend DEBUG completion -> success includes verify, audit, API and '
  'event enqueue; it is NOT API wait or quota-command duration.'
)


def timestamp(value):
  # Python 3.9 accepts microseconds, while kubectl emits nanoseconds.
  value = re.sub(r'(\.\d{6})\d+', r'\1', value)
  return datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()


def parse_logs(logs, names, start, end):
  """Use kubectl RFC3339 envelopes, not klog's yearless timestamps."""
  phases, throttles, limits, malformed = {}, 0, [], 0
  wanted = set(names)
  for pod, text in logs.items():
    for line in text.splitlines():
      try:
        stamp, body = line.split(' ', 1)
        at = timestamp(stamp)
        if not start <= at <= end:
          continue
        if 'client-side throttling' in body.lower():
          throttles += 1
        if body.startswith('{'):
          fields = json.loads(body)
          message = fields.get('msg', '')
        else:
          fields = dict(token.split('=', 1) for token in shlex.split(body) if '=' in token)
          message = fields.get('msg', '')
          if not message:
            default = re.search(r'\b(?:INFO|DEBUG) (Quota applied successfully|XFS quota applied|ext4 quota applied|Btrfs quota applied)(?= |$)', body)
            message = default.group(1) if default else ''
        if 'kube_api_qps' in fields and 'kube_api_burst' in fields:
          limits.append({'pod': pod, 'qps': fields['kube_api_qps'], 'burst': fields['kube_api_burst']})
        elif 'qps' in fields and 'burst' in fields:
          limits.append({'pod': pod, 'qps': fields['qps'], 'burst': fields['burst']})
        pv = fields.get('pv') or str(fields.get('path', '')).rsplit('/', 1)[-1]
        if pv not in wanted:
          continue
        phase = None
        if message == 'Quota applied successfully':
          phase = 'success'
        elif message in ('XFS quota applied', 'ext4 quota applied', 'Btrfs quota applied'):
          phase = 'backend_complete'
        if phase:
          phases.setdefault(pv, {}).setdefault(phase, []).append({
            'unix_seconds': at, 'pod': pod, 'correlation_id': fields.get('correlation_id')})
      except (ValueError, TypeError, AttributeError):
        malformed += 1
  for row in phases.values():
    backend, success = row.get('backend_complete', []), row.get('success', [])
    if len(backend) == len(success) == 1 and backend[0]['pod'] == success[0]['pod']:
      duration = success[0]['unix_seconds'] - backend[0]['unix_seconds']
      if duration >= 0:
        row['post_backend_seconds_including_verify_audit_api_event'] = duration
  applies = sorted(min(event['unix_seconds'] for event in row['success'])
                   for row in phases.values() if row.get('success'))
  span = applies[-1] - applies[0] if len(applies) > 1 else 0
  return {
    'unique_pvs_applied': len(applies),
    'applies_per_second': len(applies) / (end - start) if logs and end > start else None,
    'mean_consecutive_apply_gap_seconds': span / (len(applies) - 1) if len(applies) > 1 else None,
    'quota_command_seconds': None, 'api_wait_seconds': None,
    'client_side_throttling_messages': throttles,
    'effective_qps_burst_logged': limits or None,
    'malformed_log_lines': malformed, 'per_pv_phases': phases,
    'missing_instrumentation': NEEDED,
  }


def parse_metrics(text):
  values = {}
  for line in text.splitlines():
    if line.startswith('nfs_quota_agent_reconcile_'):
      parts = line.split()
      if len(parts) == 2:
        try:
          value = float(parts[1])
          if value == value and abs(value) != float('inf'):
            values[parts[0]] = value
        except ValueError:
          pass
  return values


def metric_delta(before, after, seconds):
  key = 'nfs_quota_agent_reconcile_total'
  if key not in before or key not in after or after[key] < before[key] or seconds <= 0:
    return None
  return (after[key] - before[key]) / seconds


class BurstDiagnosis:
  def __init__(self, fs, names, run):
    self.fs, self.names, self.run = fs, names, run
    self.errors, self.pods, self.before = [], [], {}
    self.directory = Path(f'/tmp/e2e-burst-diagnosis-{fs}')
    try:
      self.directory.mkdir(exist_ok=True)
      pods = json.loads(run(['kubectl', '--request-timeout=5s', 'get', 'pods', '-n',
                             'nfs-quota-agent', '-l', 'app.kubernetes.io/name=nfs-quota-agent',
                             '-o', 'json']))
      for pod in pods['items']:
        for container in pod['spec']['containers']:
          for port in container.get('ports', []):
            if port.get('name') == 'metrics':
              self.pods.append((pod['metadata']['name'], container['name'], port['containerPort']))
      if not self.pods:
        self.errors.append('No agent metrics port found; falling back to all-container logs')
        self.pods = [(pod['metadata']['name'], None, None) for pod in pods['items']]
      self.before = self.snapshot('before')
    except Exception as error:
      self.errors.append(f'discovery: {error}')
    self.start = time.time()

  def snapshot(self, suffix):
    samples = {}
    for pod, _, port in self.pods:
      if port is None:
        continue
      try:
        text = self.run(['kubectl', '--request-timeout=5s', 'get', '--raw',
                         f'/api/v1/namespaces/nfs-quota-agent/pods/{pod}:{port}/proxy/metrics'])
        (self.directory / f'{pod}-{suffix}.prom').write_text(text + '\n')
        samples[pod] = {'at': time.time(), 'values': parse_metrics(text)}
      except Exception as error:
        self.errors.append(f'{pod} metrics {suffix}: {error}')
    return samples

  def finish(self, end):
    # D4: diagnostics must never fail enforcement. Cost: missing evidence on
    # denied proxy/log access; escape: retained errors and explicit nulls.
    try:
      after = self.snapshot('after')
      logs = {}
      since = datetime.datetime.fromtimestamp(self.start, datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
      for pod, container, _ in self.pods:
        try:
          args = ['kubectl', '--request-timeout=5s', 'logs', '-n', 'nfs-quota-agent', pod,
                  '--timestamps=true', f'--since-time={since}']
          args += ['-c', container] if container else ['--all-containers=true']
          logs[pod] = self.run(args)
          (self.directory / f'{pod}.log').write_text(logs[pod] + '\n')
          print(f'BEGIN E2E BURST AGENT LOG {pod}', flush=True)
          print(logs[pod], flush=True)
          print(f'END E2E BURST AGENT LOG {pod}', flush=True)
        except Exception as error:
          self.errors.append(f'{pod} logs: {error}')
      result = parse_logs(logs, self.names, self.start, end)
      limits = result['effective_qps_burst_logged'] or []
      for pod, container, _ in self.pods:
        if any(limit['pod'] == pod for limit in limits):
          continue
        try:
          args = ['kubectl', '--request-timeout=5s', 'logs', '-n', 'nfs-quota-agent', pod,
                  '--timestamps=true']
          args += ['-c', container] if container else ['--all-containers=true']
          full_log = self.run(args)
          (self.directory / f'{pod}-full.log').write_text(full_log + '\n')
          startup = parse_logs({pod: full_log}, [], float('-inf'), float('inf'))
          limits.extend(startup['effective_qps_burst_logged'] or [])
        except Exception as error:
          self.errors.append(f'{pod} startup logs: {error}')
      result['effective_qps_burst_logged'] = limits or None
      rates = {}
      for pod in self.before.keys() & after.keys():
        first, last = self.before[pod], after[pod]
        rates[pod] = metric_delta(first['values'], last['values'], last['at'] - first['at'])
      result.update({
        'fs': self.fs, 'window_start_unix_seconds': self.start, 'window_end_unix_seconds': end,
        'reconciles_per_second': rates or None, 'metric_samples': {'before': self.before, 'after': after},
        'errors': self.errors, 'artifacts': str(self.directory),
        'limitations': 'Metrics cache up to 30s; deltas cover scrape interval and all watch '
                        'reconciles including cache hits, excluding full sync. Scrapes can run '
                        'quota reports and take a.mu. Log times are container emission times. '
                        'Zero throttle messages does not exclude throttling; normal waits may '
                        'be below logging threshold. Startup QPS/burst uses full pod logs when '
                        'absent from this window; rotated logs may omit it. Missing logs are not zero reconciles.',
      })
      (self.directory / 'diagnosis.json').write_text(json.dumps(result, indent=2) + '\n')
      result['per_pv_phases'] = str(self.directory / 'diagnosis.json')
      result['metric_samples'] = {
        phase: {pod: {'at': sample['at'], 'values': {
          key: value for key, value in sample['values'].items()
          if key in ('nfs_quota_agent_reconcile_total',
                     'nfs_quota_agent_reconcile_duration_seconds_sum')}}
                for pod, sample in samples.items()}
        for phase, samples in result['metric_samples'].items()}
    except Exception as error:
      result = {'errors': self.errors + [str(error)], 'missing_instrumentation': NEEDED}
    print('BEGIN E2E BURST DIAGNOSIS JSON', flush=True)
    print(json.dumps(result, indent=2), flush=True)
    print('END E2E BURST DIAGNOSIS JSON', flush=True)
