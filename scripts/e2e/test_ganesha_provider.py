"""Host-free process identity checks; these do not prove NFS enforcement."""

import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]


class GaneshaProviderTests(unittest.TestCase):
  def check_server(self, listeners, executable='/usr/bin/ganesha.nfsd', kernel=False):
    return subprocess.run(
      ['bash', '-c', '''
set -euo pipefail
source scripts/e2e/ganesha-provider.sh
SUDO=""
ss() { printf '%s\n' "$LISTENERS"; }
readlink() { printf '%s\n' "$EXECUTABLE"; }
pgrep() { [ "$KERNEL_NFSD" = true ]; }
cat() { echo 0; }
assert_ganesha_server
'''], cwd=ROOT, capture_output=True, text=True,
      env={**os.environ, 'LISTENERS': listeners, 'EXECUTABLE': executable,
           'KERNEL_NFSD': str(kernel).lower()})

  def test_ganesha_owns_every_listener(self):
    listener = 'LISTEN 0 4096 0.0.0.0:2049 0.0.0.0:* users:(("ganesha.nfsd",pid=42,fd=10))'
    result = self.check_server(listener + '\n' + listener)
    self.assertEqual(result.returncode, 0, result.stderr)

  def test_no_listener_fails(self):
    self.assertNotEqual(self.check_server('').returncode, 0)

  def test_kernel_nfsd_fails_even_with_ganesha_listener(self):
    result = self.check_server('users:(("ganesha.nfsd",pid=42,fd=10))', kernel=True)
    self.assertNotEqual(result.returncode, 0)
    self.assertIn('kernel nfsd', result.stderr)

  def test_wrong_or_unobservable_owner_fails(self):
    for listener in ('LISTEN 0 4096 0.0.0.0:2049 0.0.0.0:*',
                     'users:(("rpc.nfsd",pid=42,fd=10))',
                     'users:(("ganesha.nfsd",fd=10))',
                     'users:(("ganesha.nfsd",pid=42,fd=10))\nusers:(("other",pid=43,fd=11))'):
      with self.subTest(listener=listener):
        self.assertNotEqual(self.check_server(listener).returncode, 0)

  def test_spoofed_process_name_fails(self):
    result = self.check_server('users:(("ganesha.nfsd",pid=42,fd=10))', '/usr/bin/other')
    self.assertNotEqual(result.returncode, 0)
    self.assertIn('does not execute ganesha.nfsd', result.stderr)

  def test_invalid_server_switch_fails_before_host_setup(self):
    result = subprocess.run(
      ['bash', 'scripts/e2e/run-airgap-e2e.sh'], cwd=ROOT,
      capture_output=True, text=True,
      env={**os.environ, 'E2E_NFS_SERVER': 'invalid', 'E2E_CLUSTER': 'kind'})
    self.assertEqual(result.returncode, 1)
    self.assertIn('E2E_NFS_SERVER must be kernel or ganesha', result.stderr)


if __name__ == '__main__':
  unittest.main()
