#!/usr/bin/env python3
"""保存一次 Prometheus 即时计数器快照，供 service_demand.py 计算差值。"""

import argparse
import json
import time
import urllib.parse
import urllib.request


QUERIES = {
    "gateway_cpu_seconds": 'sum(process_cpu_seconds_total{job="farm-gateway"})',
    "farm_cpu_seconds": 'sum(process_cpu_seconds_total{job="farm"})',
    "social_cpu_seconds": 'sum(process_cpu_seconds_total{job="social"})',
    "gateway_alloc_bytes": 'sum(go_memstats_alloc_bytes_total{job="farm-gateway"})',
    "farm_alloc_bytes": 'sum(go_memstats_alloc_bytes_total{job="farm"})',
    "social_alloc_bytes": 'sum(go_memstats_alloc_bytes_total{job="social"})',
    "mysql_queries": 'sum(mysql_global_status_questions)',
    "redis_commands": 'sum(redis_commands_processed_total)',
}


def query(base_url, expression):
    url = base_url.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode({"query": expression})
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
    args = parser.parse_args()

    snapshot = {"captured_at": time.time(), "prometheus_url": args.url, "values": {}, "errors": {}}
    for name, expression in QUERIES.items():
        try:
            snapshot["values"][name] = query(args.url, expression)
        except Exception as error:
            snapshot["values"][name] = None
            snapshot["errors"][name] = str(error)
    with open(args.output, "w", encoding="utf-8") as output:
        json.dump(snapshot, output, ensure_ascii=False, indent=2, sort_keys=True)
        output.write("\n")


if __name__ == "__main__":
    main()
