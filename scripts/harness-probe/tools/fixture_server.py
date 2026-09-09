#!/usr/bin/env python3
"""Deterministic MCP stdio fixtures for local Harness probes.

This program deliberately implements only the small JSON-RPC surface needed by
the probe.  It never starts a shell, opens a network connection, or performs an
external effect.
"""

from __future__ import annotations

import hashlib
import json
import math
import re
import sys
import threading
import time
from typing import Any, Callable


MAX_LINE_BYTES = 64 * 1024
MAX_MARKER_BYTES = 256
MAX_REQUEST_ID_BYTES = 128
MAX_WAIT_MS = 10_000
MAX_IN_FLIGHT_REQUESTS = 8
MAX_JSON_RPC_ID_BYTES = 128
MAX_JSON_RPC_NUMBER = 9_007_199_254_740_991
REQUEST_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")
SUPPORTED_PROTOCOL_VERSIONS = frozenset({"2024-11-05", "2025-03-26", "2025-06-18"})


def tool_definition(
    name: str, description: str, properties: dict[str, Any], required_arguments: tuple[str, ...] = ()
) -> dict[str, Any]:
    return {
        "name": name,
        "description": description,
        "annotations": {"readOnlyHint": True, "destructiveHint": False,
                        "idempotentHint": True, "openWorldHint": False},
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "correlationId": {
                    "type": "string",
                    "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$",
                    "maxLength": MAX_REQUEST_ID_BYTES,
                },
                **properties,
            },
            "required": ["correlationId", *required_arguments],
        },
    }


TOOLS = (
    tool_definition(
        "marker_echo",
        "Returns a bounded marker with request/call correlation.",
        {
            "marker": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_MARKER_BYTES,
            }
        },
        ("marker",),
    ),
    tool_definition(
        "bounded_long_wait",
        "Waits for a bounded duration; cancellation is process termination.",
        {
            "delayMs": {"type": "integer", "minimum": 1, "maximum": MAX_WAIT_MS}
        },
        ("delayMs",),
    ),
    tool_definition(
        "deterministic_error",
        "Returns the same safe fixture error for every valid correlated call.",
        {},
    ),
)
ALLOWED_TOOL_NAMES = frozenset(tool["name"] for tool in TOOLS)


def rpc_error(request_id: Any, code: int, message: str, data: dict[str, Any] | None = None) -> dict[str, Any]:
    error: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        error["data"] = data
    return {"jsonrpc": "2.0", "id": request_id, "error": error}


def result(request_id: Any, value: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": request_id, "result": value}


def call_id(tool_name: str, correlation_id: str) -> str:
    digest = hashlib.sha256(f"{tool_name}:{correlation_id}".encode("ascii")).hexdigest()[:16]
    return f"fixture-{digest}"


def valid_rpc_id(value: Any) -> bool:
    if isinstance(value, str):
        return 0 < len(value.encode("utf-8")) <= MAX_JSON_RPC_ID_BYTES
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return math.isfinite(value) and abs(value) <= MAX_JSON_RPC_NUMBER


def require_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be an object")
    return value


def require_correlation_id(arguments: dict[str, Any]) -> str:
    value = arguments.get("correlationId")
    if not isinstance(value, str) or not REQUEST_ID_RE.fullmatch(value):
        raise ValueError("correlationId must be a bounded opaque identifier")
    return value


def assert_exact_keys(arguments: dict[str, Any], allowed: set[str]) -> None:
    if set(arguments) - allowed:
        raise ValueError("tool arguments contain unknown fields")


def text_content(payload: dict[str, Any]) -> list[dict[str, str]]:
    return [{"type": "text", "text": json.dumps(payload, separators=(",", ":"), sort_keys=True)}]


def tool_result(payload: dict[str, Any], *, is_error: bool = False) -> dict[str, Any]:
    value: dict[str, Any] = {"content": text_content(payload), "structuredContent": payload}
    if is_error:
        value["isError"] = True
    return value


def handle_tool_call(params: Any, emit_notification: Callable[[dict[str, Any]], None]) -> dict[str, Any]:
    call = require_object(params, "tools/call params")
    if set(call) - {"name", "arguments", "_meta"}:
        raise ValueError("tools/call contains unsupported fields")
    name = call.get("name")
    if not isinstance(name, str):
        raise ValueError("tool name must be a string")
    if name not in ALLOWED_TOOL_NAMES:
        raise LookupError("tool is not in the allowed fixture toolset")
    arguments = require_object(call.get("arguments", {}), "tool arguments")
    correlation_id = require_correlation_id(arguments)
    correlated_call_id = call_id(name, correlation_id)

    if name == "marker_echo":
        assert_exact_keys(arguments, {"correlationId", "marker"})
        marker = arguments.get("marker")
        if not isinstance(marker, str) or not marker or len(marker.encode("utf-8")) > MAX_MARKER_BYTES:
            raise ValueError("marker must be a non-empty bounded UTF-8 string")
        return tool_result({"callId": correlated_call_id, "correlationId": correlation_id, "marker": marker})

    if name == "bounded_long_wait":
        assert_exact_keys(arguments, {"correlationId", "delayMs"})
        delay_ms = arguments.get("delayMs")
        if isinstance(delay_ms, bool) or not isinstance(delay_ms, int) or not 1 <= delay_ms <= MAX_WAIT_MS:
            raise ValueError(f"delayMs must be an integer from 1 to {MAX_WAIT_MS}")
        emit_notification(
            {
                "jsonrpc": "2.0",
                "method": "notifications/message",
                "params": {
                    "level": "info",
                    "data": {
                        "callId": correlated_call_id,
                        "correlationId": correlation_id,
                        "event": "fixture_long_wait_started",
                    },
                },
            }
        )
        started = time.monotonic()
        time.sleep(delay_ms / 1000)
        elapsed_ms = int((time.monotonic() - started) * 1000)
        return tool_result(
            {"callId": correlated_call_id, "correlationId": correlation_id, "delayMs": delay_ms, "elapsedMs": elapsed_ms}
        )

    assert_exact_keys(arguments, {"correlationId"})
    return tool_result(
        {
            "callId": correlated_call_id,
            "code": "fixture_deterministic_error",
            "correlationId": correlation_id,
            "safeMessage": "deterministic fixture error",
        },
        is_error=True,
    )


def handle_request(request: Any, emit_notification: Callable[[dict[str, Any]], None]) -> dict[str, Any] | None:
    if not isinstance(request, dict):
        return rpc_error(None, -32600, "invalid request")
    request_id = request.get("id")
    is_notification = "id" not in request
    if request.get("jsonrpc") != "2.0" or not isinstance(request.get("method"), str):
        return None if is_notification else rpc_error(request_id, -32600, "invalid request")
    if not is_notification and not valid_rpc_id(request_id):
        return rpc_error(None, -32600, "request id must be a string or number")

    method = request["method"]
    if method == "notifications/initialized":
        return None
    if method == "initialize":
        params = request.get("params", {})
        if not isinstance(params, dict):
            return None if is_notification else rpc_error(request_id, -32602, "initialize params must be an object")
        protocol_version = params.get("protocolVersion", "2024-11-05")
        if protocol_version not in SUPPORTED_PROTOCOL_VERSIONS:
            return None if is_notification else rpc_error(request_id, -32602, "unsupported protocolVersion")
        return None if is_notification else result(
            request_id,
            {
                "protocolVersion": protocol_version,
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "harness-probe-fixtures", "version": "1.0.0"},
            },
        )
    if method == "ping":
        return None if is_notification else result(request_id, {})
    if method == "tools/list":
        return None if is_notification else result(request_id, {"tools": list(TOOLS)})
    if method == "tools/call":
        try:
            value = handle_tool_call(request.get("params"), emit_notification)
        except LookupError as exc:
            return None if is_notification else rpc_error(request_id, -32601, str(exc))
        except ValueError as exc:
            return None if is_notification else rpc_error(request_id, -32602, str(exc))
        return None if is_notification else result(request_id, value)
    return None if is_notification else rpc_error(request_id, -32601, "method not found")


def main() -> int:
    output_lock = threading.Lock()
    request_slots = threading.BoundedSemaphore(MAX_IN_FLIGHT_REQUESTS)

    def write(message: dict[str, Any]) -> None:
        with output_lock:
            sys.stdout.write(json.dumps(message, separators=(",", ":"), sort_keys=True) + "\n")
            sys.stdout.flush()

    def process(request: Any) -> None:
        try:
            response = handle_request(request, write)
            if response is not None:
                write(response)
        finally:
            request_slots.release()

    def busy_response(request: Any) -> dict[str, Any] | None:
        if not isinstance(request, dict) or "id" not in request:
            return None
        request_id = request.get("id")
        if not valid_rpc_id(request_id):
            return rpc_error(None, -32600, "request id must be a string or number")
        return rpc_error(
            request_id,
            -32001,
            "fixture is busy",
            {"code": "fixture_busy", "inFlightLimit": MAX_IN_FLIGHT_REQUESTS, "retryable": True},
        )

    while True:
        raw_line = sys.stdin.buffer.readline(MAX_LINE_BYTES + 1)
        if not raw_line:
            break
        if len(raw_line) > MAX_LINE_BYTES:
            while not raw_line.endswith(b"\n"):
                raw_line = sys.stdin.buffer.readline(MAX_LINE_BYTES + 1)
                if not raw_line:
                    break
            write(rpc_error(None, -32600, "request exceeds fixture input limit"))
        else:
            try:
                request = json.loads(raw_line.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                write(rpc_error(None, -32700, "parse error"))
            else:
                if request_slots.acquire(blocking=False):
                    threading.Thread(target=process, args=(request,), daemon=True).start()
                else:
                    response = busy_response(request)
                    if response is not None:
                        write(response)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
