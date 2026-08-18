#!/usr/bin/env python3
"""将编排脚本捕获的毫秒时间戳写成结构化容量实验窗口上下文。"""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--idle-start-ms", type=int, required=True)
    parser.add_argument("--idle-end-ms", type=int, required=True)
    parser.add_argument("--drain-check-start-ms", type=int, required=True)
    parser.add_argument("--journal-idle-ms", type=int, required=True)
    parser.add_argument("--recovery-end-ms", type=int, required=True)
    parser.add_argument("--recovery-seconds", type=int, required=True)
    args = parser.parse_args()
    output = {
        "run_id": args.run_id,
        "idle_start_unix_ms": args.idle_start_ms,
        "idle_end_unix_ms": args.idle_end_ms,
        "journal_drain_check_start_unix_ms": args.drain_check_start_ms,
        "journal_idle_unix_ms": args.journal_idle_ms,
        "recovery_end_unix_ms": args.recovery_end_ms,
        "configured_recovery_seconds": args.recovery_seconds,
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
