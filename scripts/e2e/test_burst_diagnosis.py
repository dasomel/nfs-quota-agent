"""Offline burst log and metrics parsing fixtures; never run kubectl."""

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from burst_diagnosis import BurstDiagnosis, metric_delta, parse_logs, parse_metrics, timestamp


class BurstParsingTests(unittest.TestCase):
  def test_json_text_phases_filter_window_and_deduplicate(self):
    start = timestamp('2026-11-10T01:00:00Z')
    logs = {'agent': '\n'.join([
      '2026-11-10T01:00:00.100000001Z time=x level=DEBUG msg="XFS quota applied" path=/export/pv-a',
      '2026-11-10T01:00:00.300Z {"msg":"Quota applied successfully","pv":"pv-a","correlation_id":"c1"}',
      '2026-11-10T01:00:00.400Z msg="Quota applied successfully" pv=pv-a',
      '2026-11-10T01:00:00.700Z msg="Quota applied successfully" pv=pv-b',
      '2026-11-10T01:00:00.800Z msg="Quota applied successfully" pv=unrelated',
      '2026-11-10T01:00:01.100Z msg="Quota applied successfully" pv=pv-c',
      '2026-11-10T01:00:00.500Z I1110 request.go:1] "Waited before sending request" reason="client-side throttling, not priority and fairness"',
      '2026-11-10T01:00:00.600Z {"msg":"config","qps":5,"burst":10}',
      'malformed',
    ])}
    result = parse_logs(logs, ['pv-a', 'pv-b', 'pv-c'], start, start + 1)
    self.assertEqual(result['unique_pvs_applied'], 2)
    self.assertEqual(result['applies_per_second'], 2)
    self.assertAlmostEqual(result['mean_consecutive_apply_gap_seconds'], .4, places=5)
    self.assertEqual(result['client_side_throttling_messages'], 1)
    self.assertEqual(result['effective_qps_burst_logged'][0]['qps'], 5)
    self.assertEqual(result['malformed_log_lines'], 1)
    self.assertIsNone(result['api_wait_seconds'])
    self.assertIsNone(result['quota_command_seconds'])
    self.assertEqual(result['per_pv_phases']['pv-a']['success'][0]['correlation_id'], 'c1')
    self.assertNotIn('post_backend_seconds_including_verify_audit_api_event', result['per_pv_phases']['pv-a'])

  def test_default_slog_handler_without_msg_field(self):
    start = timestamp('2026-11-10T00:00:00Z')
    logs = {'agent': '2026-11-10T00:00:01Z 2026/11/10 00:00:01 INFO Quota applied successfully pv=pv path=/export/pv correlation_id=c1'}
    result = parse_logs(logs, ['pv'], start, start + 2)
    self.assertEqual(result['unique_pvs_applied'], 1)
    self.assertEqual(result['per_pv_phases']['pv']['success'][0]['correlation_id'], 'c1')

  def test_invalid_json_is_counted_as_malformed_and_parsing_continues(self):
    start = timestamp('2026-11-10T00:00:00Z')
    logs = {'agent': '2026-11-10T00:00:00Z {not-json}\n'
                     '2026-11-10T00:00:01Z msg="Quota applied successfully" pv=pv'}
    result = parse_logs(logs, ['pv'], start, start + 2)
    self.assertEqual(result['malformed_log_lines'], 1)
    self.assertEqual(result['unique_pvs_applied'], 1)

  def test_debug_interval_is_not_claimed_as_api_wait(self):
    logs = {'agent': '2026-11-10T00:00:00Z msg="ext4 quota applied" path=/export/pv\n'
                     '2026-11-10T00:00:01Z msg="Quota applied successfully" pv=pv'}
    start = timestamp('2026-11-10T00:00:00Z')
    result = parse_logs(logs, ['pv'], start, start + 2)
    self.assertEqual(result['per_pv_phases']['pv']['post_backend_seconds_including_verify_audit_api_event'], 1)
    self.assertIsNone(result['api_wait_seconds'])

  def test_missing_or_single_apply_has_no_gap_or_inferred_limits(self):
    result = parse_logs({}, [], 1, 1)
    self.assertIsNone(result['mean_consecutive_apply_gap_seconds'])
    self.assertIsNone(result['effective_qps_burst_logged'])
    self.assertIsNone(result['applies_per_second'])

  def test_metrics_delta_handles_missing_reset_and_cache(self):
    key = 'nfs_quota_agent_reconcile_total'
    values = parse_metrics(f'# HELP test\n{key} 20\nother 9\n'
                           'nfs_quota_agent_reconcile_errors_total nope\n')
    self.assertEqual(values, {key: 20})
    self.assertEqual(metric_delta({key: 10}, values, 5), 2)
    self.assertEqual(metric_delta(values, values, 5), 0)
    self.assertIsNone(metric_delta(values, {key: 1}, 5))
    self.assertIsNone(metric_delta({}, values, 5))
    self.assertIsNone(metric_delta(values, values, 0))

  def test_collects_logs_and_two_read_only_metric_snapshots(self):
    calls = []
    def run(args):
      calls.append(args)
      if '--raw' in args:
        count = 12 if sum('--raw' in call for call in calls) == 2 else 10
        return f'nfs_quota_agent_reconcile_total {count}'
      if 'logs' in args:
        return '1970-01-01T00:00:02Z msg="Quota applied successfully" pv=pv'
      return json.dumps({'items': [{'metadata': {'name': 'agent'}, 'spec': {
        'containers': [{'name': 'agent', 'ports': [{'name': 'metrics', 'containerPort': 9090}]}]}}]})
    output = io.StringIO()
    with tempfile.TemporaryDirectory() as tmp, \
         patch('burst_diagnosis.Path', return_value=Path(tmp)), \
         patch('burst_diagnosis.time.time', side_effect=[1, 1, 3]), \
         contextlib.redirect_stdout(output):
      diagnosis = BurstDiagnosis('xfs', ['pv'], run)
      diagnosis.finish(3)
      result = json.loads((Path(tmp) / 'diagnosis.json').read_text())
      self.assertEqual(result['reconciles_per_second'], {'agent': 1})
      self.assertEqual(result['unique_pvs_applied'], 1)
      self.assertTrue((Path(tmp) / 'agent.log').is_file())
      self.assertTrue((Path(tmp) / 'agent-before.prom').is_file())
      self.assertTrue((Path(tmp) / 'agent-after.prom').is_file())
      self.assertEqual(result['errors'], [])
      log_call = next(call for call in calls if 'logs' in call)
      since_time = next(arg.split('=', 1)[1] for arg in log_call if arg.startswith('--since-time='))
      self.assertRegex(since_time, r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$')
      self.assertNotIn('+00:00', since_time)
    self.assertTrue(all('get' in call or 'logs' in call for call in calls))
    printed = json.loads(output.getvalue().split('JSON\n', 1)[1].rsplit('\nEND', 1)[0])
    self.assertIsInstance(printed['per_pv_phases'], str)
    self.assertEqual(printed['metric_samples']['after']['agent']['values'],
                     {'nfs_quota_agent_reconcile_total': 12})

  def test_diagnostic_failure_is_nonfatal_and_prints_json(self):
    with patch('burst_diagnosis.Path.mkdir'), patch('burst_diagnosis.time.time', return_value=1):
      diagnosis = BurstDiagnosis('xfs', ['pv'], lambda args: (_ for _ in ()).throw(RuntimeError('denied')))
    output = io.StringIO()
    with patch('burst_diagnosis.Path.write_text', side_effect=OSError('readonly')), \
         contextlib.redirect_stdout(output):
      diagnosis.finish(2)
    lines = output.getvalue().splitlines()
    self.assertEqual(lines[0], 'BEGIN E2E BURST DIAGNOSIS JSON')
    self.assertEqual(lines[-1], 'END E2E BURST DIAGNOSIS JSON')
    self.assertTrue(json.loads('\n'.join(lines[1:-1]))['errors'])


if __name__ == '__main__':
  unittest.main()
