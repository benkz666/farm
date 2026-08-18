#!/usr/bin/env python3
"""Compare production CPU extrapolation from the 3k/6k envelope and one 10k unit."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


SERVICES = ("gateway", "farm", "social", "mysql", "redis")


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline-capacity", required=True)
    parser.add_argument("--ten-thousand-window", required=True)
    parser.add_argument("--ten-thousand-slo", required=True)
    parser.add_argument("--output-json", required=True)
    parser.add_argument("--output-markdown", required=True)
    parser.add_argument("--production-qps", type=float, default=333333.33)
    parser.add_argument("--unit-qps", type=float, default=10000)
    parser.add_argument("--planning-utilization", type=float, default=0.70)
    args = parser.parse_args()

    baseline = load(args.baseline_capacity)
    window = load(args.ten_thousand_window)
    slo = load(args.ten_thousand_slo)
    if slo.get("verdict") != "pass":
        raise ValueError("10k SLO must pass before CPU comparison")

    factor = args.production_qps / args.unit_qps
    rows: dict[str, Any] = {}
    for service in SERVICES:
        derived = window["derived"]["services"][service]
        c_cpu = float(
            window["windows"]["C_load"]["services"][service]["cpu"][
                "average_cores"
            ]
        )
        a_cpu = float(derived["fixed_cpu_cores_A"] or 0.0)
        unit_variable = max(c_cpu - a_cpu, 0.0)
        required = a_cpu + factor * unit_variable
        planned = required / args.planning_utilization
        old_planned = float(
            baseline["services"][service]["cpu"]["planned_at_utilization"]
        )
        difference = (planned - old_planned) / old_planned
        rows[service] = {
            "ten_thousand_unit_variable_cpu_cores": unit_variable,
            "ten_thousand_cpu_cores_per_1000_qps": unit_variable / 10,
            "ten_thousand_production_cpu_cores": planned,
            "baseline_production_cpu_cores": old_planned,
            "relative_difference": difference,
            "conservative_selected_cpu_cores": max(planned, old_planned),
        }

    old_total = sum(row["baseline_production_cpu_cores"] for row in rows.values())
    new_total = sum(
        row["ten_thousand_production_cpu_cores"] for row in rows.values()
    )
    selected_total = sum(
        row["conservative_selected_cpu_cores"] for row in rows.values()
    )
    output = {
        "ten_thousand_slo": slo["verdict"],
        "production_scale_factor": factor,
        "planning_utilization": args.planning_utilization,
        "services": rows,
        "totals": {
            "baseline_production_cpu_cores": old_total,
            "ten_thousand_production_cpu_cores": new_total,
            "relative_difference": (new_total - old_total) / old_total,
            "conservative_selected_cpu_cores": selected_total,
        },
        "decision": "retain the larger per-service result",
    }
    Path(args.output_json).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    lines = [
        "# 10k标准单元CPU复核",
        "",
        f"- 生产放大倍数：{factor:.4f}",
        f"- 规划利用率：{args.planning_utilization:.0%}",
        f"- 10k逐接口SLO：{slo['verdict']}",
        "",
        "| 服务 | 10k单元实际CPU/核 | 原规划CPU/核 | 10k外推CPU/核 | 差异 | 保守取值/核 |",
        "| --- | ---: | ---: | ---: | ---: | ---: |",
    ]
    for service in SERVICES:
        row = rows[service]
        lines.append(
            f"| {service} | {row['ten_thousand_unit_variable_cpu_cores']:.4f} | "
            f"{row['baseline_production_cpu_cores']:.2f} | "
            f"{row['ten_thousand_production_cpu_cores']:.2f} | "
            f"{row['relative_difference'] * 100:+.2f}% | "
            f"{math.ceil(row['conservative_selected_cpu_cores'])} |"
        )
    lines.append(
        f"| **合计** | — | **{old_total:.2f}** | **{new_total:.2f}** | "
        f"**{(new_total - old_total) / old_total * 100:+.2f}%** | "
        f"**{sum(math.ceil(row['conservative_selected_cpu_cores']) for row in rows.values())}** |"
    )
    lines.extend(
        [
            "",
            "10k单元因批处理和连接复用效率更高，外推值更低。容量规划继续采用逐服务较大值，不用更乐观的10k结果下调现有CPU。",
            "",
        ]
    )
    Path(args.output_markdown).write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    main()
