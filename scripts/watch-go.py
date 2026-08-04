#!/usr/bin/env python3
"""Run one Go service and restart it when Go source files change."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


POLL_SECONDS = 0.35
STOP_TIMEOUT_SECONDS = 3


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--package", required=True)
    parser.add_argument("--label", required=True)
    return parser.parse_args()


def source_snapshot(root: Path) -> tuple[tuple[str, int, int], ...]:
    paths = list(root.rglob("*.go"))
    paths.extend(path for name in ("go.mod", "go.sum") if (path := root / name).exists())
    snapshot: list[tuple[str, int, int]] = []
    for path in paths:
        try:
            stat = path.stat()
        except FileNotFoundError:
            continue
        snapshot.append((str(path.relative_to(root)), stat.st_mtime_ns, stat.st_size))
    return tuple(sorted(snapshot))


def stop_process(process: subprocess.Popen[bytes] | None) -> None:
    if process is None or process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=STOP_TIMEOUT_SECONDS)
    except ProcessLookupError:
        return
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def main() -> int:
    args = parse_args()
    root = args.root.resolve()
    stopping = False
    process: subprocess.Popen[bytes] | None = None

    def request_stop(_signum: int, _frame: object) -> None:
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGINT, request_stop)
    signal.signal(signal.SIGTERM, request_stop)

    snapshot = source_snapshot(root)
    try:
        while not stopping:
            print(f"[{args.label}] 启动 go run {args.package}", flush=True)
            process = subprocess.Popen(
                ["go", "run", args.package],
                cwd=root,
                stdin=subprocess.DEVNULL,
                start_new_session=True,
            )
            exit_reported = False

            while not stopping:
                time.sleep(POLL_SECONDS)
                current = source_snapshot(root)
                if current != snapshot:
                    print(f"[{args.label}] 检测到 Go 代码变更，重新编译", flush=True)
                    stop_process(process)
                    process = None
                    snapshot = current
                    break
                if process.poll() is not None and not exit_reported:
                    print(
                        f"[{args.label}] 进程已退出（code={process.returncode}），"
                        "等待代码变更后重试",
                        flush=True,
                    )
                    exit_reported = True
        return 0
    finally:
        stop_process(process)


if __name__ == "__main__":
    sys.exit(main())
