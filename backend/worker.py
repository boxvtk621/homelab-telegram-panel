"""Panel-owned Cursor SDK run. Private JSONL pipes, no bot or user config.

Only explicitly provided YouTrack read tools are offered to the agent. The
parent owns credentials, authorization and HTTP; no YouTrack token enters SDK.
"""
from __future__ import annotations

import json
import os
import sys
import threading

MAX_LINE = 2 * 1024 * 1024
TOOLS = {
    "youtrack_issue": ("Read the exact issue assigned to this conversation.", {}),
    "youtrack_comments": ("Read comments of the assigned issue.", {"skip": {"type": "integer", "minimum": 0, "maximum": 100000}}),
    "youtrack_articles": ("List project Knowledge Base articles.", {"skip": {"type": "integer", "minimum": 0, "maximum": 100000}}),
    "youtrack_article": ("Read one exact project Knowledge Base article by readable ID.", {"id": {"type": "string"}}),
}


def read_line(stream):
    line = stream.readline(MAX_LINE + 1)
    if not line or len(line.encode("utf-8")) > MAX_LINE:
        raise ValueError("invalid_protocol")
    return json.loads(line)


def execute(request, sdk, emit, receive):
    lock = threading.Lock()
    sequence = 0

    def callback(name):
        def call(args, _context):
            nonlocal sequence
            with lock:
                sequence += 1
                emit({"type": "tool", "id": sequence, "name": name, "args": args})
                response = receive()
                if response.get("id") != sequence:
                    raise ValueError("invalid_protocol")
                return response["result"]
        return call

    custom = {
        name: sdk.CustomTool(
            description=description,
            input_schema={"type": "object", "properties": properties,
                          "required": list(properties), "additionalProperties": False},
            execute=callback(name),
        ) for name, (description, properties) in TOOLS.items()
    }
    options = sdk.AgentOptions(
        model=request["model"],
        tools=["mcp"],
        local=sdk.LocalAgentOptions(cwd=os.getcwd(), setting_sources=[], custom_tools=custom),
    )
    # No project/user/plugin MCP, shell, file tools or bot credential fallback.
    with sdk.Agent.create(options) as agent:
        emit({"type": "progress", "stage": "running"})
        run = agent.send(request["prompt"])
        for msg in run.messages():
            kind = getattr(msg, "type", "")
            stage = {"thinking": "analyzing", "tool_call": "reading_youtrack",
                     "assistant": "writing_answer"}.get(kind)
            if stage:
                emit({"type": "progress", "stage": stage})
        terminal = run.wait()
        result = getattr(terminal, "result", None)
        if terminal.status != "finished" or not isinstance(result, str) or not result.strip() or len(result.encode("utf-8")) > 65536:
            raise ValueError("cursor_run_failed")
        emit({"type": "result", "text": result})


def main():
    protocol = sys.stdout
    # SDK/bridge diagnostics can contain user data. They never enter the pipe
    # protocol or server logs. The parent also discards child stderr.
    sys.stdout = sys.stderr

    def emit(value):
        protocol.write(json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n")
        protocol.flush()

    try:
        request = read_line(sys.stdin)
        import cursor_sdk
        execute(request, cursor_sdk, emit, lambda: read_line(sys.stdin))
    except Exception:
        emit({"type": "error", "code": "cursor_run_failed"})
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
