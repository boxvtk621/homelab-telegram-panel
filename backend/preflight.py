"""Offline packaged SDK/bridge check; never starts an AI run or reads a key."""
import importlib.metadata
import tempfile

from cursor_sdk import CursorClient


def main():
    if importlib.metadata.version("cursor-sdk") != "1.0.31":
        raise RuntimeError("sdk_version_mismatch")
    with tempfile.TemporaryDirectory(prefix="panel-sdk-preflight-") as workspace:
        with CursorClient.launch_bridge(workspace=workspace) as client:
            client.ping()
            client.get_version()
    print("CURSOR_SDK_BRIDGE_OK (offline; no model request)")


if __name__ == "__main__":
    main()
