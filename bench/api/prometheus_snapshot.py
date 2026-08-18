#!/usr/bin/env python3
"""保存一次 Prometheus 即时计数器快照，供 service_demand.py 计算差值。"""

import argparse
import json
import time
import urllib.parse
import urllib.request


QUERIES = {
    "gateway_cpu_seconds": 'sum(process_cpu_seconds_total{service="gateway"})',
    "farm_cpu_seconds": 'sum(process_cpu_seconds_total{service="farm"})',
    "social_cpu_seconds": 'sum(process_cpu_seconds_total{service="social"})',
    "gateway_alloc_bytes": 'sum(go_memstats_alloc_bytes_total{service="gateway"})',
    "farm_alloc_bytes": 'sum(go_memstats_alloc_bytes_total{service="farm"})',
    "social_alloc_bytes": 'sum(go_memstats_alloc_bytes_total{service="social"})',
    "gateway_resident_bytes": 'sum(process_resident_memory_bytes{service="gateway"})',
    "farm_resident_bytes": 'sum(process_resident_memory_bytes{service="farm"})',
    "social_resident_bytes": 'sum(process_resident_memory_bytes{service="social"})',
    "gateway_ws_connections": 'sum(farm_ws_connections{service="gateway"})',
    "farm_actor_resident": 'sum(farm_actor_resident{service="farm"})',
    "farm_write_pending": 'sum(farm_write_journal_pending{service="farm"})',
    "farm_write_lag": 'sum(farm_write_journal_lag{service="farm"})',
    "mysql_queries": 'sum(mysql_global_status_questions)',
    "mysql_threads_connected": 'sum(mysql_global_status_threads_connected)',
    "mysql_threads_running": 'sum(mysql_global_status_threads_running)',
    "redis_commands": 'sum(redis_commands_processed_total)',
    "redis_connected_clients": 'sum(redis_connected_clients)',
    "redis_memory_used_bytes": 'sum(redis_memory_used_bytes)',
}


def query(base_url, expression, timestamp=None):
    parameters = {"query": expression}
    if timestamp is not None:
        parameters["time"] = timestamp
    url = base_url.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode(parameters)
    with urllib.request.urlopen(url, timeout=10) as response:
        body = json.loads(response.read().decode("utf-8"))
    if body.get("status") != "success":
        raise RuntimeError("Prometheus query failed: %s" % body)
    result = body.get("data", {}).get("result", [])
    if not result:
        return None
    return float(result[0]["value"][1])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:9090")
    parser.add_argument("--output", required=True)
    parser.add_argument("--timestamp", type=float, help="optional historical Unix timestamp")
    args = parser.parse_args()

    captured_at = args.timestamp if args.timestamp is not None else time.time()
    snapshot = {"captured_at": captured_at, "prometheus_url": args.url, "values": {}, "errors": {}}
    for name, expression in QUERIES.items():
        try:
            snapshot["values"][name] = query(args.url, expression, args.timestamp)
        except Exception as error:
            snapshot["values"][name] = None
            snapshot["errors"][name] = str(error)
    with open(args.output, "w", encoding="utf-8") as output:
        json.dump(snapshot, output, ensure_ascii=False, indent=2, sort_keys=True)
        output.write("\n")


if __name__ == "__main__":
    main()
