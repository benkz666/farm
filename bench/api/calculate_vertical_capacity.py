#!/usr/bin/env python3
"""按标准容量单元的保守包络外推生产逻辑资源总量。"""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--vertical-error", required=True)
    parser.add_argument("--output-json", required=True)
    parser.add_argument("--output-markdown", required=True)
    parser.add_argument("--production-qps", type=float, default=333333.33)
    parser.add_argument("--production-connections", type=float, default=4000000)
    parser.add_argument("--production-actors", type=float, default=5420000)
    parser.add_argument("--unit-qps", type=float, default=3000)
    parser.add_argument("--unit-connections", type=float, default=36000)
    parser.add_argument("--unit-actors", type=float, default=48780)
    parser.add_argument("--planning-utilization", type=float, default=0.70)
    args = parser.parse_args()

    if not 0 < args.planning_utilization <= 1:
        raise ValueError("planning utilization must be in (0, 1]")

    report = load(args.vertical_error)
    factors = {
        "business_qps": args.production_qps / args.unit_qps,
        "connections": args.production_connections / args.unit_connections,
        "resident_actors": args.production_actors / args.unit_actors,
    }
    scale = max(factors.values())
    services: dict[str, Any] = {}
    for service, row in report["services"].items():
        resources: dict[str, Any] = {}
        for resource, unit_name in (("cpu", "cores"), ("memory", "mib")):
            comparison = row[resource]
            raw = row["raw_windows"]
            baseline = max(
                float(raw["one_unit"]["A"][f"{resource}_{unit_name}"]),
                float(raw["two_unit"]["A"][f"{resource}_{unit_name}"]),
            )
            unit_variable = float(comparison["planned_unit_cost"])
            required = baseline + scale * unit_variable
            planned = required / args.planning_utilization
            resources[resource] = {
                "baseline": baseline,
                "unit_variable_cost": unit_variable,
                "linearity_pass": bool(comparison["pass"]),
                "relative_error": comparison["relative_error"],
                "required_before_headroom": required,
                "planned_at_utilization": planned,
                "method": (
                    "linear extrapolation"
                    if comparison["pass"]
                    else "conservative upper envelope; uncertainty retained"
                ),
            }
        services[service] = resources

    output = {
        "scope": "vertical logical resource totals; no HA or horizontal efficiency",
        "production_load": {
            "business_qps": args.production_qps,
            "connections": args.production_connections,
            "resident_actors": args.production_actors,
        },
        "standard_unit": {
            "business_qps": args.unit_qps,
            "connections": args.unit_connections,
            "resident_actors": args.unit_actors,
        },
        "scale_factors": factors,
        "selected_scale_factor": scale,
        "planning_utilization": args.planning_utilization,
        "services": services,
        "totals": {
            "planned_cpu_cores_rounded_per_service": sum(
                math.ceil(row["cpu"]["planned_at_utilization"])
                for row in services.values()
            ),
            "planned_memory_mib": sum(
                row["memory"]["planned_at_utilization"]
                for row in services.values()
            ),
        },
        "limitations": [
            "Failed linearity checks use the larger normalized unit cost as a conservative bound, not a precise estimate.",
            "Registration rate and server-initiated push are not closed workload inputs and are excluded.",
            "Storage capacity, IOPS and database sharding remain independent constraints.",
        ],
    }
    Path(args.output_json).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    lines = [
        "# 生产逻辑资源外推",
        "",
        f"- 放大系数：{scale:.4f}",
        f"- 规划利用率：{args.planning_utilization:.0%}",
        "- 范围：基础容量；不含容灾和水平扩展效率",
        "",
        "| 服务 | CPU单位成本/核 | CPU误差 | 规划CPU/核 | 内存单位成本/MiB | 内存误差 | 规划内存/GiB | 口径 |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |",
    ]
    for service, row in services.items():
        cpu = row["cpu"]
        memory = row["memory"]
        cpu_error = float(cpu["relative_error"] or 0) * 100
        memory_error = float(memory["relative_error"] or 0) * 100
        exact = cpu["linearity_pass"] and memory["linearity_pass"]
        lines.append(
            f"| {service} | {cpu['unit_variable_cost']:.4f} | {cpu_error:.2f}% | "
            f"{math.ceil(cpu['planned_at_utilization'])} | "
            f"{memory['unit_variable_cost']:.2f} | {memory_error:.2f}% | "
            f"{memory['planned_at_utilization'] / 1024:.2f} | "
            f"{'线性外推' if exact else '保守上界'} |"
        )
    total_cpu = sum(
        math.ceil(row["cpu"]["planned_at_utilization"])
        for row in services.values()
    )
    total_memory_gib = sum(
        row["memory"]["planned_at_utilization"] for row in services.values()
    ) / 1024
    lines.append(
        f"| **合计** | — | — | **{total_cpu}** | — | — | "
        f"**{total_memory_gib:.2f}** | — |"
    )
    lines.extend(
        [
            "",
            "未通过误差阈值的项目取两档折算单位成本的较大值，因此结果可作为保守规划上界，不能表述为高精度线性预测。",
            "",
        ]
    )
    Path(args.output_markdown).write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    main()
