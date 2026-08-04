#!/usr/bin/env python3
"""Start a command in its own session and record its PID."""

from __future__ import annotations

import argparse
from pathlib import Path
import subprocess
import sys


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--cwd", required=True, type=Path)
    parser.add_argument("--log-file", required=True, type=Path)
    parser.add_argument("--pid-file", required=True, type=Path)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()

    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        parser.error("missing command after --")

    args.log_file.parent.mkdir(parents=True, exist_ok=True)
    args.pid_file.parent.mkdir(parents=True, exist_ok=True)
    with args.log_file.open("ab", buffering=0) as output:
        process = subprocess.Popen(
            command,
            cwd=args.cwd,
            stdin=subprocess.DEVNULL,
            stdout=output,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    args.pid_file.write_text(f"{process.pid}\n")
    print(process.pid)
    return 0


if __name__ == "__main__":
    sys.exit(main())
