"""Focused contract tests for the stdio MCP probe fixtures."""

from __future__ import annotations

import json
import pathlib
import subprocess
import sys
import time
import unittest
from typing import Any


ROOT = pathlib.Path(__file__).resolve().parents[1]
SERVER = ROOT / "tools" / "fixture_server.py"


class FixtureServer:
    def __init__(self) -> None:
        self.process = subprocess.Popen(
            [sys.executable, "-u", str(SERVER)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )

    def request(self, request: dict[str, Any]) -> dict[str, Any]:
        self.send(request)
        return self.read()

    def send(self, request: dict[str, Any]) -> None:
        assert self.process.stdin is not None
        self.process.stdin.write(json.dumps(request) + "\n")
        self.process.stdin.flush()

    def read(self) -> dict[str, Any]:
        assert self.process.stdout is not None
        line = self.process.stdout.readline()
        if not line:
            raise AssertionError(f"fixture exited early: {self.process.stderr.read() if self.process.stderr else ''}")
        return json.loads(line)

    def close(self) -> None:
        if self.process.poll() is None:
            self.process.terminate()
        self.process.communicate(timeout=2)


class FixtureToolsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.server = FixtureServer()

    def tearDown(self) -> None:
        self.server.close()

    def call(self, request_id: str, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        return self.server.request(
            {
                "jsonrpc": "2.0",
                "id": request_id,
                "method": "tools/call",
                "params": {"name": name, "arguments": arguments},
            }
        )

    def test_lifecycle_and_marker_correlation(self) -> None:
        initialized = self.server.request(
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-03-26"}}
        )
        self.assertEqual("2025-03-26", initialized["result"]["protocolVersion"])
        self.server.send({"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}})
        listed = self.server.request({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
        self.assertEqual(["bounded_long_wait", "deterministic_error", "marker_echo"], sorted(tool["name"] for tool in listed["result"]["tools"]))
        listed_tools = {tool["name"]: tool["inputSchema"] for tool in listed["result"]["tools"]}
        self.assertEqual(["correlationId", "marker"], listed_tools["marker_echo"]["required"])
        self.assertEqual(["correlationId", "delayMs"], listed_tools["bounded_long_wait"]["required"])
        response = self.call("rpc-3", "marker_echo", {"correlationId": "marker-001", "marker": "m-42"})
        body = response["result"]["structuredContent"]
        self.assertEqual("marker-001", body["correlationId"])
        self.assertEqual("m-42", body["marker"])
        self.assertTrue(body["callId"].startswith("fixture-"))
        self.assertEqual(body, json.loads(response["result"]["content"][0]["text"]))

    def test_initialize_rejects_unknown_protocol_and_bad_request_id(self) -> None:
        unsupported = self.server.request(
            {"jsonrpc": "2.0", "id": "init", "method": "initialize", "params": {"protocolVersion": "2099-01-01"}}
        )
        self.assertEqual(-32602, unsupported["error"]["code"])
        bad_request_id = self.server.request({"jsonrpc": "2.0", "id": None, "method": "ping", "params": {}})
        self.assertEqual(-32600, bad_request_id["error"]["code"])
        non_finite_id = self.server.request({"jsonrpc": "2.0", "id": float("inf"), "method": "ping", "params": {}})
        self.assertEqual(-32600, non_finite_id["error"]["code"])
        oversized_id = self.server.request({"jsonrpc": "2.0", "id": "x" * 129, "method": "ping", "params": {}})
        self.assertEqual(-32600, oversized_id["error"]["code"])

    def test_deterministic_error_is_correlated_tool_error(self) -> None:
        first = self.call("rpc-1", "deterministic_error", {"correlationId": "error-001"})["result"]
        second = self.call("rpc-2", "deterministic_error", {"correlationId": "error-001"})["result"]
        self.assertTrue(first["isError"])
        self.assertEqual(first["structuredContent"], second["structuredContent"])
        self.assertEqual("fixture_deterministic_error", first["structuredContent"]["code"])

    def test_invalid_input_and_denied_effect_fail_closed(self) -> None:
        invalid = self.call("rpc-1", "marker_echo", {"correlationId": "bad space", "marker": "x"})
        self.assertEqual(-32602, invalid["error"]["code"])
        extra = self.call("rpc-2", "marker_echo", {"correlationId": "marker-001", "marker": "x", "effect": "run"})
        self.assertEqual(-32602, extra["error"]["code"])
        self.assertNotIn("effect", extra["error"]["message"])
        denied = self.call("rpc-3", "denied_effect", {"correlationId": "deny-001"})
        self.assertEqual(-32601, denied["error"]["code"])
        listed = self.server.request({"jsonrpc": "2.0", "id": 4, "method": "tools/list", "params": {}})
        self.assertNotIn("denied_effect", [tool["name"] for tool in listed["result"]["tools"]])

    def test_bounded_wait_returns_elapsed_time(self) -> None:
        started = time.monotonic()
        self.server.send({"jsonrpc": "2.0", "id": "wait", "method": "tools/call", "params": {"name": "bounded_long_wait", "arguments": {"correlationId": "wait-001", "delayMs": 40}}})
        started_event = self.server.read()
        self.assertEqual("fixture_long_wait_started", started_event["params"]["data"]["event"])
        response = self.server.read()
        elapsed = time.monotonic() - started
        body = response["result"]["structuredContent"]
        self.assertGreaterEqual(body["elapsedMs"], 30)
        self.assertLess(elapsed, 1)
        overflow = self.call("rpc-2", "bounded_long_wait", {"correlationId": "wait-002", "delayMs": 10001})
        self.assertEqual(-32602, overflow["error"]["code"])

    def test_long_wait_is_cancellable_by_process_termination(self) -> None:
        assert self.server.process.stdin is not None
        self.server.send({"jsonrpc": "2.0", "id": "wait", "method": "tools/call", "params": {"name": "bounded_long_wait", "arguments": {"correlationId": "cancel-001", "delayMs": 10000}}})
        started_event = self.server.read()
        self.assertEqual("fixture_long_wait_started", started_event["params"]["data"]["event"])
        self.server.send({"jsonrpc": "2.0", "id": "ping", "method": "ping", "params": {}})
        self.assertEqual("ping", self.server.read()["id"])
        self.server.process.terminate()
        self.server.process.wait(timeout=1)
        self.assertNotEqual(0, self.server.process.returncode)

    def test_inflight_limit_returns_structured_busy_error(self) -> None:
        for number in range(8):
            self.server.send(
                {
                    "jsonrpc": "2.0",
                    "id": f"wait-{number}",
                    "method": "tools/call",
                    "params": {
                        "name": "bounded_long_wait",
                        "arguments": {"correlationId": f"busy-{number}", "delayMs": 10000},
                    },
                }
            )
        for _ in range(8):
            self.assertEqual("fixture_long_wait_started", self.server.read()["params"]["data"]["event"])
        busy = self.server.request({"jsonrpc": "2.0", "id": "ping-while-busy", "method": "ping", "params": {}})
        self.assertEqual(-32001, busy["error"]["code"])
        self.assertEqual("fixture_busy", busy["error"]["data"]["code"])
        self.assertEqual(8, busy["error"]["data"]["inFlightLimit"])

    def test_oversized_line_is_drained_without_parsing_tail(self) -> None:
        assert self.server.process.stdin is not None
        self.server.process.stdin.write("x" * (64 * 1024 + 1) + "\n")
        self.server.process.stdin.flush()
        oversized = self.server.read()
        self.assertEqual(-32600, oversized["error"]["code"])
        ping = self.server.request({"jsonrpc": "2.0", "id": "ping-after-oversize", "method": "ping"})
        self.assertEqual("ping-after-oversize", ping["id"])


if __name__ == "__main__":
    unittest.main()
