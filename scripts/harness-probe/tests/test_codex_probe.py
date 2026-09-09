import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


MODULE = Path(__file__).parents[1] / "codex_probe.py"
SPEC = importlib.util.spec_from_file_location("codex_probe", MODULE)
codex_probe = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(codex_probe)


class FakeProtocol:
    def __init__(self, *, responses=None, notifications=None, on_request=None):
        self.responses = list(responses or [])
        self.notifications = list(notifications or [])
        self.on_request = on_request

    def request(self, method, params):
        if self.on_request is not None:
            self.on_request(method, params)
        if not self.responses:
            raise AssertionError("unexpected request")
        return self.responses.pop(0)

    def wait_until(self, predicate):
        while not predicate() and self.notifications:
            self.observer.observe(self.notifications.pop(0))


def item_event(method, turn_id, item_id, tool, status):
    return {
        "method": method,
        "params": {
            "threadId": "thread-1",
            "turnId": turn_id,
            "item": {
                "id": item_id,
                "type": "mcpToolCall",
                "server": "fixture",
                "tool": tool,
                "status": status,
                "arguments": {"correlationId": "test"},
            },
        },
    }


def turn_completed(turn_id, status):
    return {
        "method": "turn/completed",
        "params": {"threadId": "thread-1", "turn": {"id": turn_id, "status": status}},
    }


class CodexProbeTest(unittest.TestCase):
    def test_native_usage_keeps_latest_thread_total_without_double_counting(self):
        state = codex_probe.initial_state()
        observer = codex_probe.Observer(state, 'create_markers')
        for thread, total in [('first', 10), ('first', 20), ('second', 5)]:
            observer.observe({'method': 'thread/tokenUsage/updated', 'params': {
                'threadId': thread, 'turnId': 'turn-1',
                'tokenUsage': {'total': {'totalTokens': total}, 'last': {'totalTokens': 5}},
            }})
        self.assertEqual(25, sum(item['totalTokens'] for item in state['usage']))
        state['processGeneration'] += 1
        observer.observe({'method': 'thread/tokenUsage/updated', 'params': {
            'threadId': 'first', 'turnId': 'turn-2', 'tokenUsage': {'total': {'totalTokens': 3}},
        }})
        self.assertEqual(28, sum(item['totalTokens'] for item in state['usage']))

    def test_initial_policy_mismatch_blocks_every_turn(self):
        valid = lambda identifier: {'thread': {'id': identifier}, 'approvalPolicy': 'never', 'sandbox': {'type': 'readOnly'}}
        disabled = {'data': [{'name': 'shell_tool', 'enabled': False}, {'name': 'apps', 'enabled': False}]}
        for responses in (
            [{'thread': {'id': 'first'}, 'approvalPolicy': 'on-request'}],
            [valid('first'), {'thread': {'id': 'second'}, 'sandbox': {'type': 'workspaceWrite'}}],
            [valid('first'), valid('second'), {'data': []}],
            [valid('first'), valid('second'), disabled, {'data': [{'name': 'apps', 'enabled': True}]}],
        ):
            methods = []
            fake = FakeProtocol(responses=responses, on_request=lambda method, params: methods.append(method))
            state = codex_probe.initial_state()
            with self.assertRaises(codex_probe.ProbeError):
                codex_probe.create_markers(fake, state, Path('/unused'), codex_probe.Observer(state, 'create_markers'), codex_probe.policy_options('model', 'low'))
            self.assertNotIn('turn/start', methods)
            self.assertEqual(0, state['turnStartCount'])

    def test_native_agent_messages_have_no_tool_field(self):
        state = codex_probe.initial_state()
        observer = codex_probe.Observer(state, "create_markers")
        for method in ("item/started", "item/completed"):
            observer.observe({"method": method, "params": {
                "threadId": "thread-1", "turnId": "turn-1",
                "item": {"id": "message-1", "type": "agentMessage", "text": "synthetic-marker"},
            }})
        self.assertIn("synthetic-marker", observer.text("turn-1"))
        self.assertFalse(observer.forbidden_shell_seen)

    def test_wrong_turn_id_cannot_prove_same_turn_steer(self):
        state = codex_probe.initial_state()
        observer = codex_probe.Observer(state, "tool_steer_cancel")
        observer.observe(item_event("item/started", "turn-1", "item-1", "bounded_long_wait", "inProgress"))
        fake = FakeProtocol(responses=[{"turnId": "turn-2"}])
        self.assertFalse(codex_probe.steer_active_turn(fake, "thread-1", "turn-1", observer, "steer"))

    def test_interrupt_ack_is_not_terminal_evidence(self):
        state = codex_probe.initial_state()
        observer = codex_probe.Observer(state, "tool_steer_cancel")
        fake = FakeProtocol(responses=[{}], notifications=[turn_completed("turn-1", "completed")])
        fake.observer = observer
        acknowledged, terminal = codex_probe.interrupt_and_wait(fake, "thread-1", "turn-1", observer)
        self.assertTrue(acknowledged)
        self.assertEqual("completed", terminal)
        assertions = {"interruptAcknowledged": acknowledged, "terminalInterrupted": terminal == "interrupted"}
        codex_probe.set_capability(state, "cancelTerminal", assertions, {})
        self.assertEqual("unknown", state["capabilities"]["cancelTerminal"]["status"])

    def test_failure_or_unknown_cannot_pass(self):
        for status in ("unknown", "failed", "not_run"):
            state = codex_probe.initial_state()
            state["capabilities"]["markerDialogs"]["status"] = status
            self.assertEqual("unknown", codex_probe.phase_status(state, "create_markers"))
        state = codex_probe.initial_state()
        state["capabilities"]["markerDialogs"]["status"] = "observed"
        self.assertEqual("completed", codex_probe.phase_status(state, "create_markers"))

    def test_marker_leakage_false_is_required_for_observed(self):
        state = codex_probe.initial_state()
        assertions = {"firstReplyMatches": True, "secondReplyLeaks": False}
        codex_probe.set_capability(state, "markerDialogs", assertions, {})
        self.assertEqual("observed", state["capabilities"]["markerDialogs"]["status"])
        assertions["secondReplyLeaks"] = True
        codex_probe.set_capability(state, "markerDialogs", assertions, {})
        self.assertEqual("unknown", state["capabilities"]["markerDialogs"]["status"])

    def test_protocol_parse_is_bounded_and_object_only(self):
        self.assertEqual({"id": 1, "result": {}}, codex_probe.parse_protocol_line(b'{"id":1,"result":{}}'))
        with self.assertRaises(codex_probe.ProbeError):
            codex_probe.parse_protocol_line(b"x" * (codex_probe.MAX_PROTOCOL_LINE_BYTES + 1))
        with self.assertRaises(codex_probe.ProbeError):
            codex_probe.parse_protocol_line(b"[]")
        with self.assertRaises(codex_probe.ProbeError):
            codex_probe.parse_protocol_line(b"not-json")

    def test_report_drops_unrecognized_checkpoint_fields(self):
        state = codex_probe.initial_state()
        state["events"] = [{"seq": 1, "type": "turn_completed", "phase": "create_markers", "raw": "secret"}]
        state["usage"] = [{"totalTokens": 3, "raw": "secret"}]
        state["capabilities"]["markerDialogs"] = {
            "status": "unknown", "assertions": {"raw-secret": True}, "evidence": {"raw": "secret"}
        }
        report = codex_probe.build_report("create_markers", "unknown", 0, state, "model", "low")
        serialized = json.dumps(report)
        self.assertNotIn("secret", serialized)
        self.assertNotIn("raw", serialized)

    def test_policy_and_absent_effect_without_native_denial_stays_unknown(self):
        state = codex_probe.initial_state()
        assertions = {
            "shellToolDisabledInResumeRequest": True,
            "readOnlyApprovalNeverInResumeRequest": True,
            "effectiveNativePolicyConfirmed": False,
            "shellCanaryRequested": True,
            "turnTerminalCompleted": True,
            "commandExecutionAbsent": True,
            "shellCanaryAbsent": True,
        }
        codex_probe.set_capability(state, "denyAfterResume", assertions, {})
        self.assertEqual("unknown", state["capabilities"]["denyAfterResume"]["status"])

    def test_native_command_execution_is_observed_as_policy_bypass(self):
        state = codex_probe.initial_state()
        observer = codex_probe.Observer(state, "resume_deny")
        started = {
            "method": "item/started",
            "params": {
                "threadId": "thread-1", "turnId": "turn-1",
                "item": {
                    "id": "shell-1", "type": "commandExecution", "status": "inProgress",
                    "command": "touch /state/workspace/forbidden-effect",
                },
            },
        }
        completed = {
            "method": "item/completed",
            "params": {
                "threadId": "thread-1", "turnId": "turn-1",
                "item": {
                    "id": "shell-1", "type": "commandExecution", "status": "failed",
                    "error": {"code": "denied"},
                },
            },
        }
        observer.observe(started)
        observer.observe(completed)
        self.assertTrue(observer.forbidden_shell_seen)

    def test_turn_budget_is_durable_before_request_and_cumulative(self):
        with tempfile.TemporaryDirectory() as directory:
            state_path = Path(directory) / "checkpoint.json"
            state = codex_probe.initial_state()
            observed_counts = []

            def observe_request(method, params):
                self.assertEqual("turn/start", method)
                observed_counts.append(json.loads(state_path.read_text())["turnStartCount"])

            fake = FakeProtocol(
                responses=[{"turn": {"id": "turn-1"}}, {"turn": {"id": "turn-2"}}],
                on_request=observe_request,
            )
            self.assertEqual("turn-1", codex_probe.start_turn(fake, state, state_path, "thread-1", "one"))
            self.assertEqual("turn-2", codex_probe.start_turn(fake, state, state_path, "thread-1", "two"))
            self.assertEqual([1, 2], observed_counts)
            self.assertEqual(2, codex_probe.load_state(state_path)["turnStartCount"])
            state["turnStartCount"] = codex_probe.MAX_TURN_STARTS
            codex_probe.save_state(state_path, state)
            with self.assertRaisesRegex(codex_probe.ProbeError, "turn_start_budget_exhausted"):
                codex_probe.start_turn(fake, state, state_path, "thread-1", "overflow")

    def test_phase_budget_contract_is_exact(self):
        self.assertEqual(
            {"create_markers": 0, "tool_steer_cancel": 2, "resume_deny": 4},
            codex_probe.EXPECTED_BEFORE,
        )
        self.assertEqual(5, sum(codex_probe.EXPECTED_TURNS.values()))
        self.assertLessEqual(5, codex_probe.MAX_TURN_STARTS)


if __name__ == "__main__":
    unittest.main()
