#!/usr/bin/env python3
"""Exercise the real restart function with deterministic launchd timing."""
from pathlib import Path
import subprocess
import tempfile
import unittest

MOCK = r'''
comms_test_root="$1"
comms_test_case="$2"
sleep() {
  local tick
  read -r tick < "$comms_test_root/tick"
  printf '%s\n' "$((tick+1))" > "$comms_test_root/tick"
}
kill() {
  local tick
  read -r tick < "$comms_test_root/tick"
  [[ "$comms_test_case" == never_stops || "$tick" -lt 4 ]]
}
launchctl() {
  local tick count
  read -r tick < "$comms_test_root/tick"
  case "$1" in
    print)
      [[ "$comms_test_case" != fresh_error ]] || return 113
      if [[ ! -f "$comms_test_root/bootout" || "$tick" -lt 2 ]]; then
        printf 'state = running\n pid = 999999\n'; return 0
      fi
      return 113 ;;
    bootout) touch "$comms_test_root/bootout"; return 0 ;;
    bootstrap)
      read -r count < "$comms_test_root/bootstrap_count"
      count=$((count+1))
      printf '%s\n' "$count" > "$comms_test_root/bootstrap_count"
      if [[ "$comms_test_case" == fresh_error ]]; then
        printf 'bootstrap: invalid domain\n' >&2; return 64
      fi
      if [[ "$tick" -lt 4 ]]; then
        printf 'bootstrap attempted before old process exited\n' >&2; return 77
      fi
      if [[ "$comms_test_case" == persistent_eio || "$count" -eq 1 ]]; then
        printf 'Bootstrap failed: 5: Input/output error\n' >&2; return 5
      fi
      return 0 ;;
    *) return 98 ;;
  esac
}
restart_comms_launch_agent gui/501 /tmp/comms-fixture.plist
'''


class LaunchdUpgradeTest(unittest.TestCase):
    def run_case(self, case):
        source = (Path(__file__).resolve().parent / 'install.sh').read_text()
        function = source.split('# BEGIN COMMS_LAUNCHD_RESTART\n', 1)[1].split('# END COMMS_LAUNCHD_RESTART', 1)[0]
        with tempfile.TemporaryDirectory(prefix='comms-launchd-test-') as directory:
            root = Path(directory)
            (root / 'tick').write_text('0\n')
            (root / 'bootstrap_count').write_text('0\n')
            result = subprocess.run(['/bin/bash', '-c', 'set -euo pipefail\n' + function + MOCK,
                                     'test', str(root), case], capture_output=True, text=True, timeout=5)
            return result, int((root / 'bootstrap_count').read_text()), int((root / 'tick').read_text())

    def test_delayed_job_and_process_removal_then_transient_eio(self):
        result, attempts, ticks = self.run_case('delayed')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, 2)
        self.assertGreaterEqual(ticks, 5)

    def test_nontransient_error_is_not_retried_or_replaced(self):
        result, attempts, _ = self.run_case('fresh_error')
        self.assertEqual(result.returncode, 64)
        self.assertEqual(attempts, 1)
        self.assertEqual(result.stderr, 'bootstrap: invalid domain\n')

    def test_persistent_eio_is_bounded_and_preserves_error(self):
        result, attempts, _ = self.run_case('persistent_eio')
        self.assertEqual(result.returncode, 5)
        self.assertEqual(attempts, 10)
        self.assertEqual(result.stderr, 'Bootstrap failed: 5: Input/output error\n')

    def test_stuck_old_process_is_not_overlapped(self):
        result, attempts, ticks = self.run_case('never_stops')
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 0)
        self.assertEqual(ticks, 100)
        self.assertIn('did not finish stopping', result.stderr)


if __name__ == '__main__':
    unittest.main()
