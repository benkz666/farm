#!/usr/bin/env python3
"""将单次 k6 汇总与服务需求合并为固定列 CSV。"""

import argparse
import csv
import json


def metric(summary, name, key, default=""):
    value = summary.get("metrics", {}).get(name, {}).get("values", {}).get(key)
    return default if value is None else value


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--operation", required=True)
    parser.add_argument("--protocol", required=True)
    parser.add_argument("--tier", default="")
    parser.add_argument("--summary", required=True)
    parser.add_argument("--demand", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    with open(args.summary, encoding="utf-8") as source:
        summary = json.load(source)
    with open(args.demand, encoding="utf-8") as source:
        demand = json.load(source)
    latency = "api_operation_latency"
    failure_rate = metric(summary, "api_system_failure_rate", "rate")
    cpu = demand.get("cpu_ms_per_request", {}).get("total")
    allocations = demand.get("alloc_bytes_per_request", {})
    alloc_total = sum(value for value in allocations.values() if value is not None)
    if not allocations or not any(value is not None for value in allocations.values()):
        alloc_total = ""
    row = [
        args.operation, args.protocol, args.tier,
        metric(summary, latency, "avg"), metric(summary, latency, "p(95)"), metric(summary, latency, "p(99)"),
        failure_rate, "" if cpu is None else cpu,
        "" if demand.get("mysql_operations_per_request") is None else demand["mysql_operations_per_request"],
        "" if demand.get("redis_operations_per_request") is None else demand["redis_operations_per_request"],
        alloc_total, "", "",
    ]
    header = [
        "operationId", "协议", "等级", "平均ms", "P95ms", "P99ms", "错误率",
        "CPU ms/请求", "MySQL/请求", "Redis/请求", "分配B/请求", "最大稳定QPS", "瓶颈",
    ]
    with open(args.output, "w", encoding="utf-8", newline="") as target:
        writer = csv.writer(target)
        writer.writerow(header)
        writer.writerow(row)


if __name__ == "__main__":
    main()
