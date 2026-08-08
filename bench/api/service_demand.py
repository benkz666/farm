#!/usr/bin/env python3
"""按空闲速率扣除后台消耗，并输出单请求 CPU/分配/MySQL/Redis 服务需求。"""

import argparse
import json


def load(path):
    with open(path, encoding="utf-8") as source:
        return json.load(source)


def delta(start, end, name):
    left = start["values"].get(name)
    right = end["values"].get(name)
    if left is None or right is None:
        return None
    return max(0.0, right - left)


def demand(name, idle_start, idle_end, load_start, load_end, successes, scale=1.0):
    idle_delta = delta(idle_start, idle_end, name)
    load_delta = delta(load_start, load_end, name)
    if idle_delta is None or load_delta is None or successes <= 0:
        return None
    idle_seconds = max(0.001, idle_end["captured_at"] - idle_start["captured_at"])
    load_seconds = max(0.001, load_end["captured_at"] - load_start["captured_at"])
    adjusted = max(0.0, load_delta - (idle_delta / idle_seconds) * load_seconds)
    return adjusted * scale / successes


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--idle-start", required=True)
    parser.add_argument("--idle-end", required=True)
    parser.add_argument("--load-start", required=True)
    parser.add_argument("--load-end", required=True)
    parser.add_argument("--successes", type=int, required=True)
    parser.add_argument("--operation", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    idle_start, idle_end, load_start, load_end = [
        load(path) for path in (args.idle_start, args.idle_end, args.load_start, args.load_end)
    ]
    result = {
        "operationId": args.operation,
        "success_requests": args.successes,
        "cpu_ms_per_request": {},
        "alloc_bytes_per_request": {},
    }
    for service in ("gateway", "farm", "social"):
        result["cpu_ms_per_request"][service] = demand(
            service + "_cpu_seconds", idle_start, idle_end, load_start, load_end, args.successes, 1000.0
        )
        result["alloc_bytes_per_request"][service] = demand(
            service + "_alloc_bytes", idle_start, idle_end, load_start, load_end, args.successes
        )
    cpu_values = [value for value in result["cpu_ms_per_request"].values() if value is not None]
    result["cpu_ms_per_request"]["total"] = sum(cpu_values) if cpu_values else None
    result["mysql_operations_per_request"] = demand(
        "mysql_queries", idle_start, idle_end, load_start, load_end, args.successes
    )
    result["redis_operations_per_request"] = demand(
        "redis_commands", idle_start, idle_end, load_start, load_end, args.successes
    )
    with open(args.output, "w", encoding="utf-8") as output:
        json.dump(result, output, ensure_ascii=False, indent=2, sort_keys=True)
        output.write("\n")


if __name__ == "__main__":
    main()
