#!/usr/bin/env python3
"""Merge concurrent servicebench shard timing and counters for Prometheus windows."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--result", action="append", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    results = [load(path) for path in args.result]
    starts = {int(row["measurement_start_unix_ms"]) for row in results}
    durations = {int(row["measurement_millis"]) for row in results}
    if len(starts) != 1 or len(durations) != 1:
        raise ValueError(f"shards are not synchronized: starts={sorted(starts)} durations={sorted(durations)}")
    output = {
        "mode": "gateway-mixed-sharded",
        "target_qps": sum(int(row["target_qps"]) for row in results),
        "sent": sum(int(row.get("sent", 0)) for row in results),
        "succeeded": sum(int(row.get("succeeded", 0)) for row in results),
        "failed": sum(int(row.get("failed", 0)) for row in results),
        "measurement_start_unix_ms": starts.pop(),
        "measurement_millis": durations.pop(),
        "state_ready_unix_ms": max(int(row["state_ready_unix_ms"]) for row in results),
        "state_window_millis": min(int(row.get("state_window_millis", 0)) for row in results),
        "drain_millis": max(int(row.get("drain_millis", 0)) for row in results),
        "resident_actors_target": sum(int(row.get("resident_actors_target", 0)) for row in results),
        "resident_actor_refreshes": sum(int(row.get("resident_actor_refreshes", 0)) for row in results),
        "resident_actor_refresh_failures": sum(int(row.get("resident_actor_refresh_failures", 0)) for row in results),
        "connection_keepalives": sum(int(row.get("connection_keepalives", 0)) for row in results),
        "connection_keepalive_failures": sum(int(row.get("connection_keepalive_failures", 0)) for row in results),
        "sources": [str(Path(path).resolve()) for path in args.result],
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
