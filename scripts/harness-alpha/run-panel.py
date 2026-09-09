#!/usr/bin/env python3
"""Run Panel with provisioned JSON environment; never shell-evaluate config."""
import argparse
import json
import os
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("environment", type=Path)
parser.add_argument("executable", type=Path)
args = parser.parse_args()
environment = {key: value for key, value in os.environ.items() if not key.startswith("PANEL_")}
with args.environment.open() as stream:
    values = json.load(stream)
if not isinstance(values, dict) or any(not key.startswith("PANEL_") or not isinstance(value, str) for key, value in values.items()):
    parser.error("invalid Panel environment")
environment.update(values)
os.execve(args.executable.resolve(), [str(args.executable.resolve()), "serve"], environment)
