#!/usr/bin/env python3
"""Calculate per-request service demand over exact servicebench measurement windows.

Unlike point-in-time counter subtraction, Prometheus ``increase`` extrapolates
the counter to both window boundaries. This avoids attributing the preceding
warm-up scrape to a 30-second measured run.
"""

from __future__ import annotations

import argparse
import json
import urllib.parse
import urllib.request
from pathlib import Path


COUNTERS = {
    "gateway_cpu_seconds": 'process_cpu_seconds_total{service="gateway"}',
    "farm_cpu_seconds": 'process_cpu_seconds_total{service="farm"}',
    "social_cpu_seconds": 'process_cpu_seconds_total{service="social"}',
    "mysql_cpu_seconds": 'container_cpu_usage_seconds_total{namespace="benkz",container="mysql",pod=~"mysql-.*"}',
    "redis_cpu_seconds": 'container_cpu_usage_seconds_total{namespace="benkz",container="redis",pod=~"redis-.*"}',
    "gateway_alloc_bytes": 'go_memstats_alloc_bytes_total{service="gateway"}',
    "farm_alloc_bytes": 'go_memstats_alloc_bytes_total{service="farm"}',
    "social_alloc_bytes": 'go_memstats_alloc_bytes_total{service="social"}',
    "mysql_operations": "mysql_global_status_questions",
    "redis_operations": "redis_commands_processed_total",
}

# cAdvisor is scraped on a different schedule from the application exporters.
# ``increase(...[30s])`` can therefore have only one sample at an exact benchmark
# boundary.  For containers that are not restarted during one benchmark window,
# subtracting the last counter sample at the two boundaries is both stable and
# avoids shifting the measurement window forward just to catch another scrape.
BOUNDARY_COUNTERS = {"mysql_cpu_seconds", "redis_cpu_seconds"}


def query(base_url: str, expression: str, timestamp: float) -> float:
    parameters = urllib.parse.urlencode({"query": expression, "time": timestamp})
    url = base_url.rstrip("/") + "/api/v1/query?" + parameters
    with urllib.request.urlopen(url, timeout=20) as response:
        body = json.load(response)
    if body.get("status") != "success":
        raise RuntimeError(f"Prometheus query failed: {body}")
    result = body.get("data", {}).get("result", [])
    if not result:
        raise RuntimeError(f"Prometheus query returned no samples: {expression}")
    return float(result[0]["value"][1])


def load_result(path: str) -> dict:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def counter_increase(base_url: str, selector: str, result: dict) -> float:
    duration = float(result["measurement_millis"]) / 1000
    end = float(result["measurement_end_unix_ms"]) / 1000
    expression = f"sum(increase({selector}[{duration:g}s]))"
    return query(base_url, expression, end)


def boundary_counter_increase(base_url: str, selector: str, result: dict) -> float:
    start = float(result["measurement_start_unix_ms"]) / 1000
    end = float(result["measurement_end_unix_ms"]) / 1000
    start_value = query(base_url, f"sum({selector})", start)
    end_value = query(base_url, f"sum({selector})", end)
    if end_value < start_value:
        raise RuntimeError(f"counter reset inside benchmark window: {selector}")
    return end_value - start_value


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:39090")
    parser.add_argument("--idle-result", required=True)
    parser.add_argument("--load-result", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    idle = load_result(args.idle_result)
    load = load_result(args.load_result)
    successes = int(load["succeeded"])
    if successes <= 0:
        raise ValueError("load result has no successful requests")

    idle_duration = float(idle["measurement_millis"]) / 1000
    load_duration = float(load["measurement_millis"]) / 1000
    raw: dict[str, dict[str, float]] = {}
    adjusted: dict[str, float] = {}
    for name, selector in COUNTERS.items():
        increase = boundary_counter_increase if name in BOUNDARY_COUNTERS else counter_increase
        idle_increase = increase(args.url, selector, idle)
        load_increase = increase(args.url, selector, load)
        idle_scaled = idle_increase / idle_duration * load_duration
        business_increase = max(0.0, load_increase - idle_scaled)
        raw[name] = {
            "idle_increase": idle_increase,
            "load_increase": load_increase,
            "idle_scaled": idle_scaled,
            "business_increase": business_increase,
        }
        adjusted[name] = business_increase / successes

    cpu = {
        service: adjusted[f"{service}_cpu_seconds"] * 1000
        for service in ("gateway", "farm", "social", "mysql", "redis")
    }
    alloc = {
        service: adjusted[f"{service}_alloc_bytes"]
        for service in ("gateway", "farm", "social")
    }
    output = {
        "method": "Prometheus counter delta over the exact measurement window minus scaled idle baseline",
        "source_idle_result": str(Path(args.idle_result).resolve()),
        "source_load_result": str(Path(args.load_result).resolve()),
        "success_requests": successes,
        "cpu_ms_per_request": {**cpu, "total": sum(cpu.values())},
        "alloc_bytes_per_request": alloc,
        "mysql_operations_per_request": adjusted["mysql_operations"],
        "redis_operations_per_request": adjusted["redis_operations"],
        "raw_window_counters": raw,
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
