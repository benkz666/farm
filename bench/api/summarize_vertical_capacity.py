#!/usr/bin/env python3
"""Compare one-unit and two-unit resource costs before production extrapolation."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


SERVICES = ("gateway", "farm", "social", "mysql", "redis")


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def nested(value: dict[str, Any], *keys: str) -> float:
    current: Any = value
    for key in keys:
        current = current[key]
    result = float(current)
    if not math.isfinite(result):
        raise ValueError(f"non-finite metric at {'.'.join(keys)}")
    return result


def resource_windows(report: dict[str, Any], service: str) -> dict[str, dict[str, float]]:
    windows = report["windows"]
    output: dict[str, dict[str, float]] = {}
    for short, name in (("A", "A_idle"), ("B", "B_state"), ("C", "C_load")):
        row = windows[name]["services"][service]
        cpu_value = row["cpu"].get("average_cores")
        if cpu_value is None and short == "A":
            # 30秒A窗口可能只覆盖一个cAdvisor计数器样本。采集报告的
            # derived.fixed_cpu_cores_A 已按同窗口30秒rate曲线回退，不能把
            # 缺样误当成0，也不需要为此重跑整轮业务压测。
            cpu_value = report["derived"]["services"][service].get(
                "fixed_cpu_cores_A"
            )
        if cpu_value is None:
            raise ValueError(f"missing CPU metric: service={service}, window={name}")
        memory = row["memory_working_set_mib"]["aggregate"]
        memory_value = memory.get("p95")
        if memory_value is None:
            memory_value = memory.get("peak")
        output[short] = {
            "cpu_cores": float(cpu_value),
            "memory_mib": float(memory_value),
        }
    return output


def positive_delta(right: float, left: float) -> float:
    return max(right - left, 0.0)


def relative_error(left: float, right: float) -> float | None:
    denominator = max(abs(left), abs(right))
    if denominator <= 1e-12:
        return None
    return abs(left - right) / denominator


def compare_resource(
    one: dict[str, float],
    two: dict[str, float],
    key: str,
    threshold: float,
) -> dict[str, Any]:
    one_state = positive_delta(one["B"], one["A"])
    two_state = positive_delta(two["B"], two["A"]) / 2
    one_business = positive_delta(one["C"], one["B"])
    two_business = positive_delta(two["C"], two["B"]) / 2
    one_total = positive_delta(one["C"], one["A"])
    two_total = positive_delta(two["C"], two["A"]) / 2
    error = relative_error(one_total, two_total)
    return {
        "metric": key,
        "one_unit": {
            "state": one_state,
            "business": one_business,
            "total_variable": one_total,
        },
        "two_unit_normalized": {
            "state": two_state,
            "business": two_business,
            "total_variable": two_total,
        },
        "planned_unit_cost": max(one_total, two_total),
        "relative_error": error,
        "relative_error_percent": error * 100 if error is not None else None,
        "threshold_percent": threshold * 100,
        "pass": error is not None and error <= threshold,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--one-window", required=True)
    parser.add_argument("--two-window", required=True)
    parser.add_argument("--one-slo", required=True)
    parser.add_argument("--two-slo", required=True)
    parser.add_argument("--output-json", required=True)
    parser.add_argument("--output-markdown", required=True)
    parser.add_argument("--cpu-error-limit", type=float, default=0.15)
    parser.add_argument("--memory-error-limit", type=float, default=0.20)
    args = parser.parse_args()

    one_report = load(args.one_window)
    two_report = load(args.two_window)
    one_slo = load(args.one_slo)
    two_slo = load(args.two_slo)
    rows: dict[str, Any] = {}
    for service in SERVICES:
        one_windows = resource_windows(one_report, service)
        two_windows = resource_windows(two_report, service)
        one_by_resource = {
            "cpu": {name: value["cpu_cores"] for name, value in one_windows.items()},
            "memory": {name: value["memory_mib"] for name, value in one_windows.items()},
        }
        two_by_resource = {
            "cpu": {name: value["cpu_cores"] for name, value in two_windows.items()},
            "memory": {name: value["memory_mib"] for name, value in two_windows.items()},
        }
        rows[service] = {
            "cpu": compare_resource(
                one_by_resource["cpu"], two_by_resource["cpu"],
                "cpu_cores", args.cpu_error_limit,
            ),
            "memory": compare_resource(
                one_by_resource["memory"], two_by_resource["memory"],
                "memory_mib", args.memory_error_limit,
            ),
            "raw_windows": {"one_unit": one_windows, "two_unit": two_windows},
        }

    resource_pass = all(
        row[resource]["pass"]
        for row in rows.values()
        for resource in ("cpu", "memory")
    )
    slo_pass = one_slo.get("verdict") == "pass" and two_slo.get("verdict") == "pass"
    output = {
        "method": {
            "cpu": "compare C-A at 1x with (C-A)/2 at 2x",
            "memory": "compare C-A at 1x with (C-A)/2 at 2x using P95 working set",
            "relative_error": "abs(r1-r2)/max(r1,r2)",
            "planning": "take max(r1,r2) only after the error threshold passes",
        },
        "thresholds": {
            "cpu_relative_error": args.cpu_error_limit,
            "memory_relative_error": args.memory_error_limit,
        },
        "slo": {"one_unit": one_slo.get("verdict"), "two_unit": two_slo.get("verdict")},
        "services": rows,
        "resource_linearity_pass": resource_pass,
        "slo_pass": slo_pass,
        "verdict": "pass" if resource_pass and slo_pass else "fail",
        "next_step_allowed": bool(resource_pass and slo_pass),
    }
    Path(args.output_json).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    lines = [
        "# 垂直容量单元线性误差",
        "",
        "| 服务 | CPU 1倍 | CPU 2倍折算 | CPU误差 | CPU结论 | 内存1倍 MiB | 内存2倍折算 MiB | 内存误差 | 内存结论 |",
        "| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |",
    ]
    for service, row in rows.items():
        cpu = row["cpu"]
        memory = row["memory"]
        cpu_error = cpu["relative_error_percent"]
        memory_error = memory["relative_error_percent"]
        lines.append(
            f"| {service} | {cpu['one_unit']['total_variable']:.4f} | "
            f"{cpu['two_unit_normalized']['total_variable']:.4f} | "
            f"{cpu_error:.2f}% | {'通过' if cpu['pass'] else '失败'} | "
            f"{memory['one_unit']['total_variable']:.2f} | "
            f"{memory['two_unit_normalized']['total_variable']:.2f} | "
            f"{memory_error:.2f}% | {'通过' if memory['pass'] else '失败'} |"
        )
    lines.extend([
        "",
        f"- 1倍SLO：{one_slo.get('verdict')}",
        f"- 2倍SLO：{two_slo.get('verdict')}",
        f"- 资源线性：{'通过' if resource_pass else '失败'}",
        f"- 是否允许进入生产外推：{'是' if output['next_step_allowed'] else '否'}",
        "",
    ])
    Path(args.output_markdown).write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    main()
