#!/usr/bin/env python3
"""汇总 Gateway 总连接资源阶梯，拟合聚合资源曲线并区分规划点与失稳边界。"""

from __future__ import annotations

import argparse
import csv
import json
import math
import statistics
from pathlib import Path
from typing import Any


def load_json(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def percentile(values: list[float], ratio: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, math.ceil(len(ordered) * ratio) - 1))
    return ordered[index]


def linear_fit(points: list[tuple[float, float]]) -> dict[str, float] | None:
    if len(points) < 2:
        return None
    xs = [point[0] for point in points]
    ys = [point[1] for point in points]
    x_bar = statistics.fmean(xs)
    y_bar = statistics.fmean(ys)
    denominator = sum((x - x_bar) ** 2 for x in xs)
    if denominator == 0:
        return None
    slope = sum((x - x_bar) * (y - y_bar) for x, y in points) / denominator
    intercept = y_bar - slope * x_bar
    total = sum((y - y_bar) ** 2 for y in ys)
    residual = sum((y - (intercept + slope * x)) ** 2 for x, y in points)
    r_squared = 1.0 if total == 0 else max(0.0, 1.0 - residual / total)
    return {"intercept": intercept, "slope": slope, "r_squared": r_squared}


def pod_restarts(path: Path) -> int:
    if not path.exists():
        return 0
    data = load_json(path)
    return sum(
        int(status.get("restartCount", 0))
        for item in data.get("items", [])
        for status in item.get("status", {}).get("containerStatuses", [])
    )


def summarize_level(level_dir: Path, total_cpu: float, total_memory_bytes: float) -> dict[str, Any]:
    target = int(level_dir.name.split("-")[-1])
    samples_path = level_dir / "samples.csv"
    client_paths = sorted(level_dir.glob("client*.json"))
    if not samples_path.exists() or not client_paths:
        return {
            "target_connections": target,
            "protocol_stable": False,
            "planning_point": False,
            "failure": "client result or samples missing",
        }

    clients = [load_json(path) for path in client_paths]
    start_s = max(int(client["measurement_start_unix_ms"]) for client in clients) / 1000.0 + 4.0
    end_s = min(int(client["measurement_end_unix_ms"]) for client in clients) / 1000.0 - 4.0
    raw_rows: list[dict[str, str]] = []
    with samples_path.open(newline="", encoding="utf-8") as handle:
        raw_rows.extend(csv.DictReader(handle))

    # One scrape cycle writes one row per Gateway sequentially. A cycle can
    # cross a wall-clock second boundary, so grouping only by unix_s would turn
    # one pool sample into two incomplete samples. Reconstruct cycles by the
    # repeated, sorted pod sequence instead.
    expected_pods = {row["pod"] for row in raw_rows}
    cycles: list[list[dict[str, str]]] = []
    current: dict[str, dict[str, str]] = {}
    for row in raw_rows:
        pod = row["pod"]
        if pod in current:
            if len(current) == len(expected_pods):
                cycles.append(list(current.values()))
            current = {}
        current[pod] = row
        if len(current) == len(expected_pods):
            cycles.append(list(current.values()))
            current = {}

    aggregates: list[dict[str, float]] = []
    for rows in cycles:
        timestamp = statistics.fmean(float(row["unix_s"]) for row in rows)
        if not (start_s <= timestamp <= end_s):
            continue
        aggregates.append(
            {
                "time": float(timestamp),
                "connections": sum(float(row["ws_connections"]) for row in rows),
                "rss": sum(float(row["gateway_rss_bytes"]) for row in rows),
                "cpu": sum(float(row["gateway_cpu_seconds"]) for row in rows),
                "fds": sum(float(row["gateway_open_fds"]) for row in rows),
                "heap": sum(float(row["gateway_heap_inuse_bytes"]) for row in rows),
                "goroutines": sum(float(row["gateway_goroutines"]) for row in rows),
                "closed": sum(float(row["gateway_closed_total"]) for row in rows),
                "loadgen_memory": max(float(row["loadgen_memory_bytes"]) for row in rows),
                "loadgen_cpu": max(float(row["loadgen_cpu_usec"]) for row in rows),
                "max_pod_connections": max(float(row["ws_connections"]) for row in rows),
                "min_pod_connections": min(float(row["ws_connections"]) for row in rows),
            }
        )
    if len(aggregates) < 2:
        return {
            "target_connections": target,
            "protocol_stable": False,
            "planning_point": False,
            "failure": "insufficient steady samples",
        }

    cpu_rates = [
        (right["cpu"] - left["cpu"]) / (right["time"] - left["time"])
        for left, right in zip(aggregates, aggregates[1:])
        if right["time"] > left["time"] and right["cpu"] >= left["cpu"]
    ]
    loadgen_cpu_rates = [
        (right["loadgen_cpu"] - left["loadgen_cpu"])
        / 1_000_000
        / (right["time"] - left["time"])
        for left, right in zip(aggregates, aggregates[1:])
        if right["time"] > left["time"] and right["loadgen_cpu"] >= left["loadgen_cpu"]
    ]
    min_connections = min(row["connections"] for row in aggregates)
    max_connections = max(row["connections"] for row in aggregates)
    closed_delta = aggregates[-1]["closed"] - aggregates[0]["closed"]
    restarts_before = pod_restarts(level_dir / "pods-before.json")
    restarts_after = pod_restarts(level_dir / "pods-after.json")
    restart_delta = max(0, restarts_after - restarts_before)
    client_failed = sum(int(client.get("failed", 0)) for client in clients)
    sent = sum(int(client.get("sent", 0)) for client in clients)
    succeeded = sum(int(client.get("succeeded", 0)) for client in clients)
    error_rate = client_failed / sent if sent else 1.0
    retention = min_connections / target
    protocol_stable = (
        retention >= 0.999
        and error_rate <= 0.001
        and max(float(client.get("p90_ms", math.inf)) for client in clients) < 300
        and max(float(client.get("p99_ms", math.inf)) for client in clients) < 500
        and restart_delta == 0
        and closed_delta <= max(1.0, target * 0.001)
    )
    rss_p95 = percentile([row["rss"] for row in aggregates], 0.95)
    cpu_p95 = percentile(cpu_rates, 0.95)
    planning_point = (
        protocol_stable
        and rss_p95 <= total_memory_bytes * 0.70
        and cpu_p95 <= total_cpu * 0.70
    )
    return {
        "target_connections": target,
        "measurement_samples": len(aggregates),
        "minimum_retained_connections": min_connections,
        "maximum_connections": max_connections,
        "retention_ratio": retention,
        "client": {
            "shards": len(clients),
            "sent": sent,
            "succeeded": succeeded,
            "failed": client_failed,
            "error_rate": error_rate,
            "actual_qps": sum(float(client.get("actual_qps", 0)) for client in clients),
            "average_ms": (
                sum(float(client.get("average_ms", 0)) * int(client.get("sent", 0)) for client in clients)
                / sent
                if sent
                else 0.0
            ),
            "p90_ms_conservative_max_across_shards": max(float(client.get("p90_ms", 0)) for client in clients),
            "p99_ms_conservative_max_across_shards": max(float(client.get("p99_ms", 0)) for client in clients),
        },
        "gateway_pool": {
            "rss_bytes_p95": rss_p95,
            "rss_bytes_max": max(row["rss"] for row in aggregates),
            "heap_bytes_p95": percentile([row["heap"] for row in aggregates], 0.95),
            "cpu_cores_average": statistics.fmean(cpu_rates) if cpu_rates else 0.0,
            "cpu_cores_p95": cpu_p95,
            "open_fds_p95": percentile([row["fds"] for row in aggregates], 0.95),
            "goroutines_p95": percentile([row["goroutines"] for row in aggregates], 0.95),
            "closed_connections_delta": closed_delta,
            "max_pod_connections": max(row["max_pod_connections"] for row in aggregates),
            "min_pod_connections": min(row["min_pod_connections"] for row in aggregates),
            "restart_delta": restart_delta,
        },
        "load_generator": {
            "memory_bytes_max": max(row["loadgen_memory"] for row in aggregates),
            "cpu_cores_p95": percentile(loadgen_cpu_rates, 0.95),
        },
        "protocol_stable": protocol_stable,
        "planning_point": planning_point,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--result-dir", required=True)
    parser.add_argument("--total-gateway-cpu", type=float, required=True)
    parser.add_argument("--total-gateway-memory-gib", type=float, required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    root = Path(args.result_dir)
    levels = [
        summarize_level(
            path,
            args.total_gateway_cpu,
            args.total_gateway_memory_gib * 1024**3,
        )
        for path in sorted(root.glob("level-*"), key=lambda item: int(item.name.split("-")[-1]))
    ]
    stable = [row for row in levels if row.get("protocol_stable")]
    planning = [row for row in levels if row.get("planning_point")]
    curve_rows = planning if len(planning) >= 2 else stable
    memory_fit = linear_fit(
        [(row["target_connections"], row["gateway_pool"]["rss_bytes_p95"] / 1024**2) for row in curve_rows]
    )
    cpu_fit = linear_fit(
        [(row["target_connections"], row["gateway_pool"]["cpu_cores_average"]) for row in curve_rows]
    )
    fd_fit = linear_fit(
        [(row["target_connections"], row["gateway_pool"]["open_fds_p95"]) for row in curve_rows]
    )
    output = {
        "method": "aggregate Gateway resource pool connection curve; instance count is not a capacity input",
        "gateway_resource_pool": {
            "cpu_cores": args.total_gateway_cpu,
            "memory_gib": args.total_gateway_memory_gib,
        },
        "acceptance": {
            "retention_ratio_min": 0.999,
            "technical_error_rate_max": 0.001,
            "ping_p90_ms_max": 300,
            "ping_p99_ms_max": 500,
            "planning_cpu_utilization_max": 0.70,
            "planning_memory_utilization_max": 0.70,
        },
        "levels": levels,
        "highest_observed_protocol_stable_connections": max(
            (row["target_connections"] for row in stable), default=None
        ),
        "highest_observed_planning_connections": max(
            (row["target_connections"] for row in planning), default=None
        ),
        "first_observed_failed_connections": min(
            (row["target_connections"] for row in levels if not row.get("protocol_stable")),
            default=None,
        ),
        "resource_curve": {
            "fit_source": "planning points" if len(planning) >= 2 else "protocol-stable points",
            "memory_mib": memory_fit,
            "cpu_cores": cpu_fit,
            "open_fds": fd_fit,
        },
        "interpretation": [
            "resource curve coefficients feed the aggregate CPU and memory pool calculation",
            "the observed stable/failure bracket is only a nonlinear process/topology validation boundary",
            "production instance count is selected after aggregate CPU and memory are known",
        ],
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
