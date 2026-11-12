"""Synthetic timing fixtures only; no cluster access."""

import unittest

from binding_diagnosis import binding_summary, controller_flags, record_bounds


class BindingDiagnosisTests(unittest.TestCase):
  def summary(self, binds, applies):
    observations = {str(i): {'observed_unix_seconds': at, 'latency_ms': at * 1000,
                               'creation_timestamp': '1970-01-01T00:00:00Z'}
                    for i, at in enumerate(applies)}
    return binding_summary(observations, {str(i): at for i, at in enumerate(binds)}, len(applies))

  def test_agent_side_when_all_bind_early_and_lag_grows(self):
    result = self.summary([1] * 10, list(range(2, 12)))
    self.assertEqual(result['serializer_reading'], 'agent-side')
    self.assertEqual(result['first_create_to_last_bound_seconds'], 1)
    self.assertEqual(result['mean_consecutive_bound_gap_seconds'], 0)
    self.assertIsNone(result['binds_per_second'])
    self.assertEqual(result['apply_minus_bound_ms'], {'p50': 5000, 'p90': 9000, 'p99': 10000, 'max': 10000})

  def test_binder_side_with_same_poll_ties(self):
    result = self.summary([1, 1, 2, 2, 3, 3], [1, 1, 2.5, 2.5, 3, 3])
    self.assertEqual(result['serializer_reading'], 'binder-side')
    self.assertEqual(result['binds_per_second'], 2.5)
    self.assertEqual(result['observed_applies_per_second'], 2.5)
    self.assertEqual(result['mean_consecutive_bound_gap_seconds'], .4)
    self.assertEqual(result['bound_and_applied_same_poll_count'], 4)

  def test_inconclusive_when_neither_pattern_matches(self):
    result = self.summary(list(range(1, 7)), list(range(3, 9)))
    self.assertEqual(result['serializer_reading'], 'inconclusive')

  def test_missing_negative_empty_and_one_observation_are_inconclusive(self):
    for binds, applies in (([], []), ([1], [1]), ([1], [1, 2, 3, 4, 5]),
                           ([2, 3, 4, 5, 6], [1, 2, 3, 4, 5])):
      with self.subTest(binds=binds):
        self.assertEqual(self.summary(binds, applies)['serializer_reading'], 'inconclusive')

  def test_rule_requires_early_binding_and_growing_lag(self):
    self.assertEqual(self.summary([1] * 6, [2] * 6)['serializer_reading'], 'inconclusive')
    self.assertEqual(self.summary([1, 2, 3, 4, 5, 6], [2, 3, 4, 5, 6, 7])[
      'serializer_reading'], 'inconclusive')  # p90 exactly two intervals, not below

  def test_one_apply_poll_has_no_invented_order(self):
    result = self.summary([1, 1, 2, 2, 3, 3], [10] * 6)
    self.assertEqual(result['serializer_reading'], 'inconclusive')

  def test_bound_is_first_observation_and_independent_of_applied(self):
    bounds = {}
    pending = {'metadata': {'name': 'pv', 'annotations': {'nfs.io/quota-status': 'applied'}}}
    record_bounds([pending], bounds, 1)
    self.assertEqual(bounds, {})
    bound = {**pending, 'status': {'phase': 'Bound'}}
    record_bounds([bound], bounds, 2)
    record_bounds([bound], bounds, 3)
    self.assertEqual(bounds, {'pv': 2})

  def test_controller_flags_both_forms_and_absent_defaults(self):
    result = controller_flags('kube-controller-manager\n--kube-api-qps=50\n'
                              '--kube-api-burst\n100\n--concurrent-pv-syncs=5\n--v=2')
    self.assertEqual(result['rate_flags'], ['--kube-api-qps=50', '--kube-api-burst 100',
                                           '--concurrent-pv-syncs=5'])
    self.assertEqual(controller_flags('kube-controller-manager --v=2')['rate_flags'], [])
    self.assertIn('defaults', result['absent_flags'])


if __name__ == '__main__':
  unittest.main()
