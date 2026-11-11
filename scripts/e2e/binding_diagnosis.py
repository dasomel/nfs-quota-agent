"""Offline Bound/apply observation analysis; thresholds are diagnostic only."""

import datetime
import itertools
import math
import shlex
import statistics


POLL_SECONDS = 0.5
RULE = (
  'Requires every requested PV Bound and applied, >=5 PVs, and nonnegative lags. '
  'agent-side: last Bound <= median apply - 2 poll intervals, and median lag '
  'in the later half of distinct apply-poll groups exceeds the earlier half '
  'by >=2 intervals, with >=75% of adjacent group median lags nondecreasing. '
  'binder-side: otherwise p90 lag '
  '<2 intervals and first-to-last observation rates differ by <=20% of apply rate. '
  'Otherwise inconclusive. Rates=(N-1)/(last-first); ties have zero gaps.'
)


def record_bounds(items, bounds, observed):
  for pv in items:
    if pv.get('status', {}).get('phase') == 'Bound':
      bounds.setdefault(pv['metadata']['name'], observed)


def controller_flags(command):
  tokens = shlex.split(command)
  flags = []
  for index, token in enumerate(tokens):
    name = token.split('=', 1)[0]
    if name in ('--kube-api-qps', '--kube-api-burst') or name.startswith('--concurrent-'):
      flags.append(token if '=' in token else ' '.join(tokens[index:index + 2]))
  return {'rate_flags': flags, 'absent_flags': 'Absent flags use binary defaults; values not inferred.'}


def binding_summary(observations, bounds, expected_count):
  rows = list(observations.values())
  applies = sorted(row['observed_unix_seconds'] for row in rows)
  binds = sorted(bounds.values())
  paired = sorted((row['observed_unix_seconds'],
                   row['observed_unix_seconds'] - bounds[name])
                  for name, row in observations.items() if name in bounds)
  lags = [lag for _, lag in paired]
  ordered_ms = sorted(lag * 1000 for lag in lags)

  def rate(times):
    return (len(times) - 1) / (times[-1] - times[0]) if len(times) > 1 and times[-1] > times[0] else None

  bind_rate, apply_rate = rate(binds), rate(applies)
  reading = 'inconclusive'
  complete = len(rows) == len(paired) == len(binds) == expected_count and expected_count >= 5
  if complete and min(lags) >= 0:
    group_lags = [statistics.median(lag for _, lag in group)
                  for _, group in itertools.groupby(paired, key=lambda pair: pair[0])]
    half = len(group_lags) // 2
    grows = (half > 0
             and statistics.median(group_lags[half:]) - statistics.median(group_lags[:half]) >= 2 * POLL_SECONDS
             and sum(b >= a for a, b in zip(group_lags, group_lags[1:])) / (len(group_lags) - 1) >= .75)
    if binds[-1] <= statistics.median(applies) - 2 * POLL_SECONDS and grows:
      reading = 'agent-side'
    elif (ordered_ms[math.ceil(len(lags) * .9) - 1] < 2 * POLL_SECONDS * 1000
          and bind_rate is not None and apply_rate is not None
          and abs(bind_rate - apply_rate) <= .2 * apply_rate):
      reading = 'binder-side'
  # D5: keep polling unchanged to avoid perturbing binder/API throughput.
  # Cost: sub-poll ordering is unknowable; escape: a separate finer-resolution run.
  return {
    'bound_pvs_observed': len(binds),
    'binds_per_second': bind_rate, 'observed_applies_per_second': apply_rate,
    'rate_definition': '(N-1)/(last-first), separately for Bound and applied LIST observations',
    'mean_consecutive_bound_gap_seconds': (binds[-1] - binds[0]) / (len(binds) - 1) if len(binds) > 1 else None,
    'first_create_to_last_bound_seconds': binds[-1] - min(
      datetime.datetime.fromisoformat(row['creation_timestamp'].replace('Z', '+00:00')).timestamp()
      for row in rows) if binds and rows else None,
    'apply_minus_bound_ms': {
      **{f'p{p}': ordered_ms[math.ceil(len(ordered_ms) * p / 100) - 1] if ordered_ms else None
         for p in (50, 90, 99)}, 'max': max(ordered_ms) if ordered_ms else None},
    'bound_and_applied_same_poll_count': sum(lag == 0 for lag in lags),
    'serializer_reading': reading, 'serializer_rule': RULE,
    'poll_interval_seconds': POLL_SECONDS,
    'polling_limits': '0.5s sleep plus LIST duration; first observations, not transition times. '
                      'Same-poll order is unknown; ties limit trend evidence. API creationTimestamp '
                      'has 1s resolution and uses a different clock. 0.2s rejected: 2.5x LIST load. '
                      'Reading is a heuristic, not proof of causality; missing/negative pairs are inconclusive.',
  }
