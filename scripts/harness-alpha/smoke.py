#!/usr/bin/env python3
"""Bounded native chat smoke through the actual mTLS Harness API (three sends)."""
import argparse
import json
from pathlib import Path
import ssl
import time
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    root = args.directory
    cfg = json.loads((root / "node.json").read_text())
    tls = ssl.create_default_context(cafile=str(root / "ca.pem"))
    tls.load_cert_chain(str(root / "gateway.pem"), str(root / "gateway.key"))
    base = "https://" + cfg["listen"] + "/v1/nodes/" + cfg["nodeId"]

    def request(path, body=None):
        req = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(),
            headers={"X-Harness-Actor-ID": cfg["ownerId"], "Content-Type": "application/json"})
        with urllib.request.urlopen(req, context=tls, timeout=15) as response:
            result = json.load(response)
            if body is not None:
                assert response.status == 202, result
            return result

    def command(kind, expected, payload, **target):
        value = {"protocolVersion": 1, "schemaId": "harness-wire-v1", "commandId": str(uuid.uuid4()),
            "kind": kind, "target": {"nodeId": cfg["nodeId"], **target}, "expected": expected, "payload": payload}
        # No retry: a lost acknowledgement must be resolved before another send.
        result = request("/commands", value)
        print(json.dumps({"stage": kind, "receipt": result}), flush=True)
        return result

    def until(read, predicate, label):
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            value = read()
            if predicate(value):
                return value
            time.sleep(1)
        raise RuntimeError("timeout: " + label)

    snapshot = request("/snapshot")
    assert snapshot["node"]["occupancy"] == "idle", snapshot
    receipt = command("dialog.create", {"registryVersion": 1}, {"title": "Alpha acceptance"})
    dialog = receipt["references"]["dialogId"]

    def enqueue(text):
        listing = request("/dialogs")
        current = next(d for d in listing["items"] if d["dialogId"] == dialog)
        return command("message.enqueue", {"dialogVersion": current["version"]}, {"text": text}, dialogId=dialog)

    def finished(receipt):
        request_id = receipt["references"]["requestId"]
        result = until(lambda: request("/requests/" + request_id + "/attempts"),
            lambda p: p["items"] and p["items"][-1]["state"] in ("completed", "failed", "interrupted", "unknown"), "terminal")
        print(json.dumps({"stage": "terminal", "attempt": result["items"][-1]}), flush=True)
        assert result["items"][-1]["state"] == "completed", result
        history = request("/dialogs/" + dialog + "/messages")
        print(json.dumps({"stage": "history", "value": history}), flush=True)
        attempt_id = result["items"][-1]["attemptId"]
        replies = [m for m in history["items"] if m["role"] == "assistant" and m["attemptId"] == attempt_id]
        assert len(replies) == 1 and replies[0]["content"].get("content", "").strip() == "КЕДР-482", replies
        return history

    finished(enqueue("Запомни контрольное слово КЕДР-482. Ответь только этим словом."))
    finished(enqueue("Какое контрольное слово я просил запомнить? Ответь только им."))
    receipt = enqueue("Напиши подробный рассказ о путешествии по лесу, не менее 5000 слов.")
    request_id = receipt["references"]["requestId"]
    snapshot = until(lambda: request("/snapshot"), lambda s: s.get("activeAttempt") is not None and
        s["activeAttempt"]["requestId"] == request_id and s["activeAttempt"]["state"] == "running", "running")
    attempt = snapshot["activeAttempt"]
    command("attempt.stop", {"attemptGeneration": attempt["generation"]}, {}, attemptId=attempt["attemptId"])
    result = until(lambda: request("/snapshot"), lambda s: s.get("activeAttempt") is None and s["node"]["queuePaused"], "stopped")
    stopped = request("/requests/" + request_id + "/attempts")
    assert any(a["attemptId"] == attempt["attemptId"] and a["state"] == "interrupted" for a in stopped["items"]), stopped
    print(json.dumps({"stage": "stopped", "attempts": stopped}), flush=True)
    print(json.dumps({"stage": "PASS", "sends": 3, "dialogId": dialog, "snapshot": result}), flush=True)


if __name__ == "__main__":
    main()
