"""Read native app-server account kind/model catalog; never request a turn."""
import json
import os
import re
import selectors
import subprocess
import sys
import time


def main():
    check_tools = sys.argv[1:] == ["--check-tools"]
    if check_tools:
        from codex_probe import prepare_policy_config
        prepare_policy_config("gpt-5.6-luna", "low")
    process = subprocess.Popen(
        ["codex", "-c", 'forced_login_method="chatgpt"', "app-server"],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    )
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    pending = bytearray()
    deadline = time.monotonic() + 40

    def send(identifier, method, params):
        request = {"method": method, "params": params}
        if identifier is not None:
            request["id"] = identifier
        process.stdin.write(json.dumps(request).encode() + b"\n")
        process.stdin.flush()

    def response(identifier):
        while time.monotonic() < deadline:
            if b"\n" not in pending:
                if not selector.select(timeout=1):
                    continue
                chunk = os.read(process.stdout.fileno(), 8192)
                if not chunk:
                    raise ValueError("catalog_transport")
                pending.extend(chunk)
                if len(pending) > 1024 * 1024:
                    raise ValueError("catalog_output_limit")
            while b"\n" in pending:
                line, _, remainder = pending.partition(b"\n")
                pending[:] = remainder
                value = json.loads(line)
                if value.get("id") == identifier:
                    if "error" in value:
                        code = value["error"].get("code") if isinstance(value["error"], dict) else None
                        suffix = str(code) if type(code) is int and -999999 <= code <= 999999 else "unknown"
                        raise ValueError("catalog_rpc_error_" + str(identifier) + "_" + suffix)
                    return value["result"]
        raise ValueError("catalog_timeout")

    try:
        send(1, "initialize", {"clientInfo": {"name": "hl240_probe", "version": "0.1.0"}})
        response(1)
        send(None, "initialized", {})
        send(2, "account/read", {"refreshToken": False})
        account = response(2).get("account") or {}
        kind = account.get("type")
        if kind != "chatgpt":
            raise ValueError("subscription_login_not_confirmed")
        send(3, "model/list", {"limit": 100, "includeHidden": False})
        catalog = response(3)
        models = []
        for item in catalog.get("data", []):
            identifier = item.get("model", item.get("id"))
            if not isinstance(identifier, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", identifier):
                continue
            efforts = [entry.get("reasoningEffort") for entry in item.get("supportedReasoningEfforts", [])]
            models.append({"model": identifier, "reasoningEfforts": [effort for effort in efforts if effort in {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}]})
        report = {"stage": "V3_catalog", "engine": "codex-app-server", "status": "completed", "authMode": kind, "modelCalls": 0, "models": models}
        if check_tools:
            send(4, "mcpServerStatus/list", {"limit": 20})
            server_result = response(4)
            servers = server_result.get("data", [])
            fixture = next((server for server in servers if server.get("name") == "fixture"), {})
            tools = fixture.get("tools", {})
            names = set(tools) if isinstance(tools, dict) else {item.get("name") for item in tools if isinstance(item, dict)}
            expected = {"marker_echo", "bounded_long_wait", "deterministic_error"}
            report["fixtureToolsObserved"] = sorted(names & expected)
            report["fixtureToolsetExact"] = names == expected
            report["unexpectedServerCount"] = sum(server.get("name") != "fixture" for server in servers)
            report["catalogComplete"] = server_result.get("nextCursor") is None
            if names != expected or report["unexpectedServerCount"] or not report["catalogComplete"]:
                report["status"] = "unknown"
        print(json.dumps(report))
    finally:
        process.terminate()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
        selector.close()


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        code = str(exc)
        allowed = {"catalog_transport", "catalog_output_limit", "catalog_timeout", "subscription_login_not_confirmed", "codex_home_missing", "codex_home_outside_state", "fixture_server_missing"}
        if code not in allowed and not re.fullmatch(r"catalog_rpc_error_[1-4]_(?:-?[0-9]{1,6}|unknown)", code):
            code = "native_catalog_failed"
        print(json.dumps({"stage": "V3_catalog", "status": "failed", "code": code, "modelCalls": 0}))
        sys.exit(1)
