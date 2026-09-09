import copy
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).parents[1]))
import codex_run


def completed(phase):
    probe = codex_run.probe
    state = probe.initial_state()
    before = probe.EXPECTED_BEFORE[phase]
    state['turnStartCount'] = before + probe.EXPECTED_TURNS[phase]
    for name in probe.PHASE_CAPABILITIES[phase]:
        state['capabilities'][name] = {
            'status': 'observed',
            'assertions': {key: key not in probe.FALSE_ASSERTIONS for key in probe.CAPABILITY_ASSERTIONS[name]},
            'evidence': {'threadIds': ['thread-1'], 'turnIds': ['turn-1']},
        }
    return probe.build_report(phase, 'completed', before, state, 'gpt-5.6-luna', 'low')


class CodexRunTest(unittest.TestCase):
    def validate(self, value, phase='create_markers'):
        return codex_run.validate_report(value, phase, codex_run.probe.EXPECTED_BEFORE[phase], 'gpt-5.6-luna', 'low')

    def test_false_or_missing_assertions_never_pass(self):
        for phase in codex_run.probe.PHASES:
            valid = completed(phase)
            self.validate(valid, phase)
            for name in codex_run.probe.PHASE_CAPABILITIES[phase]:
                for assertion, expected in valid['capabilities'][name]['assertions'].items():
                    forged = copy.deepcopy(valid)
                    forged['capabilities'][name]['assertions'][assertion] = not expected
                    with self.subTest(phase=phase, assertion=assertion), self.assertRaises(ValueError):
                        self.validate(forged, phase)
                missing = copy.deepcopy(valid)
                missing['capabilities'][name]['assertions'] = {}
                with self.assertRaises(ValueError):
                    self.validate(missing, phase)

    def test_raw_fields_wrong_identity_and_unknown_counts_are_rejected(self):
        for mutate in (
            lambda r: r.update(raw='secret'),
            lambda r: r.update(events=[{'raw': 'secret'}]),
            lambda r: r.update(usage=[{'totalTokens': 1, 'raw': 'secret'}]),
            lambda r: r.update(countBefore=True),
            lambda r: r.update(countAfter='unknown'),
            lambda r: r.update(countAfter=9),
            lambda r: r.update(model='wrong'),
            lambda r: r['capabilities']['markerDialogs'].update(evidence={}),
        ):
            value = completed('create_markers')
            mutate(value)
            with self.assertRaises(ValueError):
                self.validate(value)

    def test_unknown_phase_preserves_known_usage_and_count(self):
        value = completed('create_markers')
        value['status'] = 'unknown'
        value['capabilities']['markerDialogs']['status'] = 'unknown'
        value['capabilities']['markerDialogs']['assertions']['firstReplyMatches'] = False
        value['usage'] = [{'totalTokens': 42}]
        self.assertEqual(2, self.validate(value))

    def test_native_auth_is_separate_from_disposable_state(self):
        command = codex_run.phase_command('desktop-linux', 'sha256:fixed', 'owned', 'owned-state', 'fresh-auth-state', 'create_markers', 'gpt-5.6-luna', 'low')
        mounts = [command[index + 1] for index, value in enumerate(command) if value == '--mount']
        self.assertEqual(['type=volume,source=owned-state,target=/state', 'type=volume,source=fresh-auth-state,target=/state/auth'], mounts)
        self.assertIn('CODEX_HOME=/state/auth/home/codex', command)
        self.assertIn('--read-only', command)
        self.assertEqual('512m', command[command.index('--memory') + 1])
        self.assertEqual('10001:10001', command[command.index('--user') + 1])
        self.assertFalse(any('type=bind' in value or 'API_KEY' in value or 'docker.sock' in value for value in command))


if __name__ == '__main__':
    unittest.main()
