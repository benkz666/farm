#!/usr/bin/env python3
"""把容量实验JSON渲染为可直接粘贴进报告的Markdown表格。"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


SERVICES = ("gateway", "farm", "social", "mysql", "redis")


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def percent(value: float) -> str:
    return f"{value * 100:.3f}%"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", required=True)
    parser.add_argument("--candidate", required=True)
    parser.add_argument("--cost-summary", required=True)
    parser.add_argument("--capacity", required=True)
    args = parser.parse_args()

    model = load(args.model)
    candidate = load(args.candidate)
    costs = load(args.cost_summary)
    capacity = load(args.capacity)

    print("| 接口 | 实际QPS | 成功/发送 | 错误率 | Avg | P90 | P99 |")
    print("| ---- | ------: | --------: | -----: | --: | --: | --: |")
    for operation in model["operations"]:
        name = operation["name"]
        step = candidate["steps"][name]
        sent = int(step["sent"])
        failed = int(step["failed"])
        print(
            f"| `{name}` | {float(step['actual_qps']):.2f} | {sent - failed:,}/{sent:,} | "
            f"{percent(failed / sent if sent else 0.0)} | {float(step['average_ms']):.2f} ms | "
            f"{float(step['p90_ms']):.2f} ms | {float(step['p99_ms']):.2f} ms |"
        )

    print("\n| 成本档 | 成功/发送 | 整体P90 | 整体P99 | Gateway峰值 | Farm峰值 | Social峰值 | MySQL峰值 | Redis峰值 |")
    print("| -----: | --------: | ------: | ------: | ----------: | -------: | ---------: | --------: | --------: |")
    for run in costs["runs"]:
        service = run["services"]
        peaks = [percent(float(service[name]["cpu_peak_per_pod_ratio"])) for name in SERVICES]
        print(
            f"| {int(run['target_qps']):,} QPS | {int(run['succeeded']):,}/{int(run['sent']):,} | "
            f"{float(run['p90_ms']):.2f} ms | {float(run['p99_ms']):.2f} ms | "
            + " | ".join(peaks)
            + " |"
        )

    print("\n| 服务 | 成本档CPU/千QPS | 拓扑校验CPU/千QPS | 最终规划CPU/千QPS | 规划内存/千QPS | 固定CPU/实例 | 固定内存/实例 | 状态CPU | 状态内存 |")
    print("| ---- | ----------------: | ------------------: | ------------------: | ----------------: | -----------: | ------------: | -------: | -------: |")
    for name in SERVICES:
        coeff = costs["coefficients"][name]
        capacity_row = capacity["baseline"]["services"][name]
        state_label = (
            "每连接"
            if name == "gateway"
            else "每在线租约"
            if name == "redis"
            else "每Actor"
            if name == "farm"
            else "—"
        )
        state_cpu = coeff.get("state_cpu_cores_per_unit_max")
        state_mem = coeff.get("state_memory_mib_per_unit_max")
        print(
            f"| {name} | {float(capacity_row['measured_cpu_cores_per_1000_qps']):.4f} 核 | "
            f"{float(capacity_row['cross_topology_cpu_cores_per_1000_qps']):.4f} 核 | "
            f"{float(capacity_row['planning_cpu_cores_per_1000_qps']):.4f} 核 | "
            f"{float(coeff['planning_memory_mib_per_1000_qps']):.2f} MiB | "
            f"{float(coeff['fixed_cpu_cores_per_instance_max'] or 0):.4f} 核 | "
            f"{float(coeff['fixed_memory_mib_per_instance_max'] or 0):.2f} MiB | "
            f"{state_label + ' ' + format(float(state_cpu), '.3e') + ' 核' if state_cpu is not None else '—'} | "
            f"{state_label + ' ' + format(float(state_mem), '.6f') + ' MiB' if state_mem is not None else '—'} |"
        )

    print("\n| 服务 | CPU约束 | 内存约束 | 密度约束 | 其他约束 | 基础实例/主分片 | 单实例规格 | 申请CPU | 申请内存 | 主约束 |")
    print("| ---- | -------: | -------: | -------: | -------- | --------------: | ---------- | ------: | -------: | ------ |")
    for name in SERVICES:
        row = capacity["baseline"]["services"][name]
        constraints = row["constraints"]
        other = ", ".join(
            f"{key}={value}" for key, value in constraints.items() if key not in ("cpu", "memory", "validated_business_density")
        ) or "—"
        spec = row["instance_spec"]
        spec_text = f"{spec['cpu_cores']}C/{spec['memory_gib']}GiB"
        print(
            f"| {name} | {constraints['cpu']} | {constraints['memory']} | "
            f"{constraints['validated_business_density']} | {other} | "
            f"{row['baseline_instances_or_primary_shards']} | {spec_text} | "
            f"{row['requested_cpu_cores']:.0f} 核 | {row['requested_memory_gib']:.0f} GiB | "
            f"{', '.join(row['binding_constraints'])} |"
        )

    print("\n| 服务 | 基础实例/主分片 | 容灾实例/总副本 | 单实例规格 | 容灾CPU | 容灾内存 |")
    print("| ---- | ----------------: | ----------------: | ---------- | ------: | -------: |")
    for name in SERVICES:
        baseline_row = capacity["baseline"]["services"][name]
        recovery_row = capacity["disaster_recovery"]["services"][name]
        spec = baseline_row["instance_spec"]
        spec_text = f"{spec['cpu_cores']}C/{spec['memory_gib']}GiB"
        print(
            f"| {name} | {baseline_row['baseline_instances_or_primary_shards']} | "
            f"{recovery_row['disaster_recovery_instances']} | {spec_text} | "
            f"{recovery_row['requested_cpu_cores']:.0f} 核 | "
            f"{recovery_row['requested_memory_gib']:.0f} GiB |"
        )


if __name__ == "__main__":
    main()
