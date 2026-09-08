"""Contract tests: no network, credentials, private data or actual agent run."""
import io
import types
import unittest

import worker


class SDKContractTest(unittest.TestCase):
    def test_synthetic_sdk_contract_and_read_only_tools(self):
        events = []
        seen = {}
        def options(**kwargs):
            return types.SimpleNamespace(**kwargs)
        class Agent:
            @staticmethod
            def create(opts):
                seen["options"] = opts
                return Agent()
            def __enter__(self):
                return self
            def __exit__(self, *_args):
                pass
            def send(self, prompt):
                seen["prompt"] = prompt
                seen["tool"] = seen["options"].local.custom_tools["youtrack_article"].execute({"id": "HL-A-23"}, None)
                return types.SimpleNamespace(
                    messages=lambda: iter([types.SimpleNamespace(type="thinking"), types.SimpleNamespace(type="assistant")]),
                    wait=lambda: types.SimpleNamespace(status="finished", result="Synthetic result"),
                )
        sdk = types.SimpleNamespace(Agent=Agent, AgentOptions=options, LocalAgentOptions=options, CustomTool=options)
        worker.execute({"model": "test-model", "prompt": "synthetic"}, sdk, events.append, lambda: {"id": 1, "result": {"data": "synthetic article"}})
        self.assertEqual(seen["options"].tools, ["mcp"])
        self.assertEqual(seen["options"].local.setting_sources, [])
        self.assertEqual(set(seen["options"].local.custom_tools), set(worker.TOOLS))
        self.assertEqual(seen["tool"], {"data": "synthetic article"})
        self.assertEqual(events[-1], {"type": "result", "text": "Synthetic result"})
        self.assertEqual(seen["prompt"], "synthetic")

    def test_missing_or_oversized_protocol_is_rejected(self):
        for text in ["", "x" * (worker.MAX_LINE + 1)]:
            with self.assertRaises(ValueError):
                worker.read_line(io.StringIO(text))


if __name__ == "__main__":
    unittest.main()
