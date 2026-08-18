#!/usr/bin/env python3
"""把只读MySQL/Redis采样转换成容量公式可复用的每账号系数。"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def parse_info(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or ":" not in line:
            continue
        key, value = line.split(":", 1)
        values[key] = value
    return values


def integer(value: str | None) -> int | None:
    try:
        return int(value or "")
    except ValueError:
        return None


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sample-dir", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    sample_dir = Path(args.sample_dir)
    account_count = int((sample_dir / "mysql-account-count.txt").read_text().strip())
    tables: list[dict[str, Any]] = []
    mysql_bytes = 0
    for line in (sample_dir / "mysql-tables.tsv").read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        name, rows, data_length, index_length, data_free = line.split("\t")
        row = {
            "table": name,
            "estimated_rows": int(rows),
            "data_bytes": int(data_length),
            "index_bytes": int(index_length),
            "free_bytes": int(data_free),
        }
        row["used_bytes"] = row["data_bytes"] + row["index_bytes"]
        mysql_bytes += row["used_bytes"]
        tables.append(row)

    redis_info = parse_info(sample_dir / "redis-info-memory.txt")
    redis_dataset = integer(redis_info.get("used_memory_dataset"))
    redis_used = integer(redis_info.get("used_memory"))
    redis_rss = integer(redis_info.get("used_memory_rss"))
    patterns: dict[str, int] = {}
    for index, line in enumerate(
        (sample_dir / "redis-key-patterns.tsv").read_text(encoding="utf-8").splitlines()
    ):
        if index == 0 or not line.strip():
            continue
        pattern, count = line.split("\t")
        patterns[pattern] = int(count)

    output = {
        "method": "read-only information_schema and Redis INFO sample",
        "sample_dir": str(sample_dir.resolve()),
        "test_accounts": account_count,
        "mysql": {
            "tables": tables,
            "data_plus_index_bytes": mysql_bytes,
            "data_plus_index_bytes_per_account": mysql_bytes / account_count,
            "note": "Used pages divided by test accounts; production disk must separately add growth, binlog, temporary space and backup headroom.",
        },
        "redis": {
            "used_memory_bytes": redis_used,
            "used_memory_dataset_bytes": redis_dataset,
            "used_memory_rss_bytes": redis_rss,
            "dataset_bytes_per_active_test_account_upper_bound": (
                redis_dataset / account_count if redis_dataset is not None else None
            ),
            "key_patterns": patterns,
            "note": "This is an upper bound for the active Redis working set because it includes TTL caches, sessions and journal metadata; scale it by active cached accounts/Actors, not all registered accounts.",
        },
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
