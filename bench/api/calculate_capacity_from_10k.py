#!/usr/bin/env python3
"""Extrapolate production logical resources from the selected 10k capacity unit."""

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
    parser.add_argument("--window", required=True)
    parser.add_argument("--slo", required=True)
    parser.add_argument("--output-json", required=True)
    parser.add_argument("--output-markdown", required=True)
    parser.add_argument("--production-qps", type=float, default=333333.33)
    parser.add_argument("--unit-qps", type=float, default=10000)
    parser.add_argument("--planning-utilization", type=float, default=0.70)
    args = parser.parse_args()

    window = load(args.window)
    slo = load(args.slo)
    if slo.get("verdict") != "pass":
        raise ValueError("10k interface SLO must pass before extrapolation")

    factor = args.production_qps / args.unit_qps
    rows: dict[str, Any] = {}
    for service in SERVICES:
        derived = window["derived"]["services"][service]
        service_windows = window["windows"]
        fixed_cpu = float(derived["fixed_cpu_cores_A"] or 0.0)
        state_cpu = float(derived["state_cpu_cores_B_minus_A"] or 0.0)
        complete_cpu_ms = float(
            derived["complete_cpu_milliseconds_per_successful_request"]
        )
        # complete CPU includes the D-window async projection work caused by C.
        required_cpu = (
            fixed_cpu
            + factor * state_cpu
            + args.production_qps * complete_cpu_ms / 1000
        )
        planned_cpu = required_cpu / args.planning_utilization

        a_memory = float(
            service_windows["A_idle"]["services"][service][
                "memory_working_set_mib"
            ]["aggregate"]["p95"]
        )
        c_memory = float(
            service_windows["C_load"]["services"][service][
                "memory_working_set_mib"
            ]["aggregate"]["p95"]
        )
        unit_variable_memory = max(c_memory - a_memory, 0.0)
        required_memory = a_memory + factor * unit_variable_memory
        planned_memory = required_memory / args.planning_utilization
        rows[service] = {
            "fixed_cpu_cores": fixed_cpu,
            "unit_state_cpu_cores": state_cpu,
            "complete_cpu_milliseconds_per_request": complete_cpu_ms,
            "required_cpu_cores_before_headroom": required_cpu,
            "planned_cpu_cores": planned_cpu,
            "unit_memory_baseline_mib": a_memory,
            "unit_memory_peak_mib": c_memory,
            "unit_variable_memory_mib": unit_variable_memory,
            "required_memory_mib_before_headroom": required_memory,
            "planned_memory_mib": planned_memory,
        }

    total_cpu = sum(row["planned_cpu_cores"] for row in rows.values())
    total_memory = sum(row["planned_memory_mib"] for row in rows.values())
    output = {
        "selected_standard_unit": {
            "business_qps": args.unit_qps,
            "connections": 120000,
            "resident_actors": 162600,
        },
        "production_scale_factor": factor,
        "planning_utilization": args.planning_utilization,
        "interface_slo": slo["verdict"],
        "services": rows,
        "totals": {
            "planned_cpu_cores": total_cpu,
            "planned_cpu_cores_rounded_per_service": sum(
                math.ceil(row["planned_cpu_cores"]) for row in rows.values()
            ),
            "planned_memory_mib": total_memory,
        },
        "limitations": [
            "The 10k interface SLO passed.",
            "The measured Journal/Projection lag drained 47.699 seconds after load; no additional sustainability rerun is included by current scope.",
            "Storage capacity, IOPS, registration and server push remain separate constraints.",
        ],
    }
    Path(args.output_json).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    lines = [
        "# 10k标准容量单元生产外推",
        "",
        f"- 放大倍数：{factor:.4f}",
        f"- 规划利用率：{args.planning_utilization:.0%}",
        f"- 接口SLO：{slo['verdict']}",
        "",
        "| 服务 | 10k完整CPU ms/请求 | 70%前CPU/核 | 规划CPU/核 | 70%前内存/GiB | 规划内存/GiB |",
        "| --- | ---: | ---: | ---: | ---: | ---: |",
    ]
    for service in SERVICES:
        row = rows[service]
        lines.append(
            f"| {service} | {row['complete_cpu_milliseconds_per_request']:.4f} | "
            f"{row['required_cpu_cores_before_headroom']:.2f} | "
            f"{math.ceil(row['planned_cpu_cores'])} | "
            f"{row['required_memory_mib_before_headroom'] / 1024:.2f} | "
            f"{row['planned_memory_mib'] / 1024:.2f} |"
        )
    lines.append(
        f"| **合计** | — | **{total_cpu * args.planning_utilization:.2f}** | "
        f"**{sum(math.ceil(row['planned_cpu_cores']) for row in rows.values())}** | "
        f"**{total_memory * args.planning_utilization / 1024:.2f}** | "
        f"**{total_memory / 1024:.2f}** |"
    )
    lines.extend(
        [
            "",
            "CPU包含C窗口请求处理和D窗口异步投影排空成本；内存按10k单元A到C的工作集增量外推。",
            "",
        ]
    )
    Path(args.output_markdown).write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    main()
