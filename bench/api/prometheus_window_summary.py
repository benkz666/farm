#!/usr/bin/env python3
"""Summarize Prometheus resource and backlog metrics for one servicebench result window."""

from __future__ import annotations

import argparse
import json
import statistics
import urllib.parse
import urllib.request
from pathlib import Path


QUERIES = {
    "gateway_cpu_percent": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="gateway",pod=~"gateway-.*"}[30s])) * 100',
    "farm_cpu_percent": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="farm",pod=~"farm-.*"}[30s])) * 100',
    "social_cpu_percent": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="social",pod=~"social-.*"}[30s])) * 100',
    "mysql_cpu_percent": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="mysql",pod=~"mysql-.*"}[30s])) * 100',
    "redis_cpu_percent": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="redis",pod=~"redis-.*"}[30s])) * 100',
    "gateway_memory_mib": 'sum(container_memory_working_set_bytes{namespace="benkz",container="gateway",pod=~"gateway-.*"}) / 1048576',
    "farm_memory_mib": 'sum(container_memory_working_set_bytes{namespace="benkz",container="farm",pod=~"farm-.*"}) / 1048576',
    "social_memory_mib": 'sum(container_memory_working_set_bytes{namespace="benkz",container="social",pod=~"social-.*"}) / 1048576',
    "mysql_memory_mib": 'sum(container_memory_working_set_bytes{namespace="benkz",container="mysql",pod=~"mysql-.*"}) / 1048576',
    "redis_memory_mib": 'sum(container_memory_working_set_bytes{namespace="benkz",container="redis",pod=~"redis-.*"}) / 1048576',
    "gateway_throttle_percent": '100 * sum(rate(container_cpu_cfs_throttled_periods_total{namespace="benkz",container="gateway",pod=~"gateway-.*"}[30s])) / clamp_min(sum(rate(container_cpu_cfs_periods_total{namespace="benkz",container="gateway",pod=~"gateway-.*"}[30s])), 1e-9)',
    "farm_throttle_percent": '100 * sum(rate(container_cpu_cfs_throttled_periods_total{namespace="benkz",container="farm",pod=~"farm-.*"}[30s])) / clamp_min(sum(rate(container_cpu_cfs_periods_total{namespace="benkz",container="farm",pod=~"farm-.*"}[30s])), 1e-9)',
    "social_throttle_percent": '100 * sum(rate(container_cpu_cfs_throttled_periods_total{namespace="benkz",container="social",pod=~"social-.*"}[30s])) / clamp_min(sum(rate(container_cpu_cfs_periods_total{namespace="benkz",container="social",pod=~"social-.*"}[30s])), 1e-9)',
    "gateway_ws_connections": 'sum(farm_ws_connections{service="gateway"})',
    "gateway_ws_rate_limited_per_second": 'sum(rate(farm_ws_rate_limited_total{service="gateway"}[30s]))',
    "farm_actor_resident": 'sum(farm_actor_resident{service="farm"})',
    "farm_write_pending": 'sum(farm_write_journal_pending{service="farm"})',
    "farm_write_lag": 'sum(farm_write_journal_lag{service="farm"})',
    "farm_projection_active": 'sum(farm_write_journal_projection_active{service="farm"})',
    "farm_barrier_waiters": 'sum(farm_write_journal_barrier_waiters{service="farm"})',
    "farm_stream_queue_depth": 'sum(farm_grpc_stream_queue_depth{service="farm"})',
    "farm_stream_in_flight": 'sum(farm_grpc_stream_in_flight{service="farm"})',
    "farm_stream_rejected_per_second": 'sum(rate(farm_grpc_stream_rejected_total{service="farm"}[30s]))',
    "farm_write_admission_limit": 'sum(farm_write_admission_limit{service="farm"})',
    "farm_write_admission_rejected_per_second": 'sum(rate(farm_write_admission_rejected_total{service="farm"}[30s]))',
    "farm_committer_batches_per_second": 'sum(rate(farm_committer_batches_total{service="farm"}[30s]))',
    "farm_committer_requests_per_second": 'sum(rate(farm_committer_requests_total{service="farm"}[30s]))',
    "farm_write_journal_appends_per_second": 'sum(rate(farm_write_journal_appends_total{service="farm"}[30s]))',
    "farm_write_journal_append_records_per_second": 'sum(rate(farm_write_journal_append_records_total{service="farm"}[30s]))',
    "farm_write_journal_projection_batches_per_second": 'sum(rate(farm_write_journal_projection_batches_total{service="farm"}[30s]))',
    "farm_write_journal_projection_records_per_second": 'sum(rate(farm_write_journal_projection_records_total{service="farm"}[30s]))',
    "mysql_questions_per_second": 'sum(rate(mysql_global_status_questions[30s]))',
    "mysql_threads_connected": 'sum(mysql_global_status_threads_connected)',
    "mysql_threads_running": 'sum(mysql_global_status_threads_running)',
    "mysql_row_lock_waits_per_second": 'sum(rate(mysql_global_status_innodb_row_lock_waits[30s]))',
    "redis_commands_per_second": 'sum(rate(redis_commands_processed_total[30s]))',
    "redis_connected_clients": 'sum(redis_connected_clients)',
    "redis_used_memory_mib": 'sum(redis_memory_used_bytes) / 1048576',
}


def query_range(base_url: str, expression: str, start: float, end: float, step: int) -> list[float]:
    parameters = urllib.parse.urlencode({"query": expression, "start": start, "end": end, "step": step})
    url = base_url.rstrip("/") + "/api/v1/query_range?" + parameters
    with urllib.request.urlopen(url, timeout=20) as response:
        body = json.load(response)
    if body.get("status") != "success":
        raise RuntimeError(f"Prometheus query failed: {body}")
    values: list[float] = []
    for series in body.get("data", {}).get("result", []):
        values.extend(float(sample[1]) for sample in series.get("values", []))
    return values


def summarize(values: list[float]) -> dict[str, float | int | None]:
    return {
        "samples": len(values),
        "average": statistics.mean(values) if values else None,
        "peak": max(values) if values else None,
        "last": values[-1] if values else None,
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:39090")
    parser.add_argument("--result", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--step", type=int, default=5)
    args = parser.parse_args()

    result = json.loads(Path(args.result).read_text(encoding="utf-8"))
    start = float(result["measurement_start_unix_ms"]) / 1000
    # Prometheus scrapes every 15 seconds. Include one trailing scrape so the
    # final counter increment enters 30-second rate calculations.
    end = float(result["measurement_end_unix_ms"]) / 1000 + 15
    output = {
        "source_result": str(Path(args.result).resolve()),
        "measurement_start": start,
        "measurement_end": float(result["measurement_end_unix_ms"]) / 1000,
        "prometheus_query_end": end,
        "scrape_interval_seconds": 15,
        "metrics": {},
        "errors": {},
    }
    for name, expression in QUERIES.items():
        try:
            output["metrics"][name] = summarize(query_range(args.url, expression, start, end, args.step))
        except Exception as error:  # Preserve partial evidence if one exporter is unavailable.
            output["metrics"][name] = summarize([])
            output["errors"][name] = str(error)

    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
