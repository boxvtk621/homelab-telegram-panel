from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).parents[1]))
import cursor_deny_run


class CursorDenyRunTest(unittest.TestCase):
    def test_diagnostic_preserves_uncertainty_without_dropping_reservation(self):
        value = {'stage': 'V2_deny', 'phase': 'resume', 'status': 'unknown', 'sdkSendCountBefore': 8, 'sdkSendCountAfter': 9,
                 'diagnostic': {'code': 'native_probe_failed', 'operation': 'send', 'errorClass': 'ConnectError'}}
        cursor_deny_run.validate(value, 'resume', 8)
        value['status'] = 'observed'
        with self.assertRaises(ValueError):
            cursor_deny_run.validate(value, 'resume', 8)
        value['status'] = 'unknown'
        value['diagnostic']['raw'] = 'secret'
        with self.assertRaises(ValueError):
            cursor_deny_run.validate(value, 'resume', 8)

    def test_diagnostic_cannot_reset_or_exceed_count(self):
        for after in (7, 10, 'unknown', True):
            value = {'stage': 'V2_deny', 'phase': 'resume', 'status': 'unknown', 'sdkSendCountBefore': 8, 'sdkSendCountAfter': after,
                     'diagnostic': {'code': 'phase_deadline', 'operation': 'send', 'errorClass': 'UnknownError'}}
            with self.assertRaises(ValueError):
                cursor_deny_run.validate(value, 'resume', 8)


if __name__ == '__main__':
    unittest.main()
