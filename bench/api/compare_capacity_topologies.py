#!/usr/bin/env python3
"""在相同QPS和总资源下比较两种服务拆分方式的单位CPU成本。"""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


SERVICES = ("gateway", "farm", "social", "mysql", "redis")


def load(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def finite(value: Any) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return number if math.isfinite(number) else None


def unit_cpu(prom: dict[str, Any], service: str, complete: bool) -> float | None:
    qps = finite(prom.get("derived", {}).get("successful_qps"))
    derived = prom.get("derived", {}).get("services", {}).get(service, {})
    if qps is None or qps <= 0:
        return None
    if complete:
        seconds = finite(derived.get("complete_business_cpu_seconds"))
        duration = finite(prom.get("windows", {}).get("C_load", {}).get("duration_seconds"))
        if seconds is None or duration is None or duration <= 0:
            return None
        return seconds / duration * 1000 / qps
    cores = finite(derived.get("business_cpu_cores_C_minus_B"))
    return cores * 1000 / qps if cores is not None else None


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline-run", required=True)
    parser.add_argument("--alternate-run", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    baseline_dir = Path(args.baseline_run)
    alternate_dir = Path(args.alternate_run)
    baseline_client = load(baseline_dir / "client.json")
    alternate_client = load(alternate_dir / "client.json")
    baseline_prom = load(baseline_dir / "prometheus-windows.json")
    alternate_prom = load(alternate_dir / "prometheus-windows.json")
    if int(baseline_client["target_qps"]) != int(alternate_client["target_qps"]):
        raise ValueError("topology cross-check runs must use the same target QPS")

    rows: dict[str, Any] = {}
    overall_pass = True
    conservative = False
    for service in SERVICES:
        baseline = unit_cpu(baseline_prom, service, complete=True)
        alternate = unit_cpu(alternate_prom, service, complete=True)
        baseline_sync = unit_cpu(baseline_prom, service, complete=False)
        alternate_sync = unit_cpu(alternate_prom, service, complete=False)
        relative = (
            abs(alternate - baseline) / baseline
            if baseline is not None and alternate is not None and baseline > 0
            else None
        )
        signed_penalty = (
            (alternate - baseline) / baseline
            if baseline is not None and alternate is not None and baseline > 0
            else None
        )
        if relative is None or signed_penalty is None:
            grade = "invalid"
            reusable = False
        elif signed_penalty <= 0:
            grade = "alternate no worse; baseline cost is conservative"
            reusable = True
        elif signed_penalty <= 0.10:
            grade = "small (<=10%)"
            reusable = True
        elif signed_penalty <= 0.20:
            grade = "visible (10%-20%)"
            reusable = True
        else:
            # 用户选择不继续重复拓扑实验时，不能把>20%的已观测惩罚忽略。
            # 规划值直接取较大成本，并在生产公式中继续保留gamma折扣。
            grade = "large (>20%); absorbed by larger cost plus horizontal discount"
            reusable = True
            conservative = True
        overall_pass = overall_pass and reusable
        rows[service] = {
            "baseline_cpu_cores_per_1000_qps": baseline,
            "alternate_cpu_cores_per_1000_qps": alternate,
            "relative_difference": relative,
            "signed_alternate_penalty": signed_penalty,
            "baseline_synchronous_cpu_cores_per_1000_qps": baseline_sync,
            "alternate_synchronous_cpu_cores_per_1000_qps": alternate_sync,
            "synchronous_relative_difference": (
                abs(alternate_sync - baseline_sync) / baseline_sync
                if baseline_sync is not None
                and alternate_sync is not None
                and baseline_sync > 0
                else None
            ),
            "grade": grade,
            "reusable": reusable,
            "cross_topology_planning_cpu_per_1000_qps": max(
                value for value in (baseline, alternate) if value is not None
            )
            if baseline is not None or alternate is not None
            else None,
        }

    output = {
        "method": "same QPS and same aggregate resources, different instance split",
        "target_qps": int(baseline_client["target_qps"]),
        "baseline_run": str(baseline_dir.resolve()),
        "alternate_run": str(alternate_dir.resolve()),
        "services": rows,
        "verdict": (
            "pass_with_conservative_cost"
            if overall_pass and conservative
            else "pass"
            if overall_pass
            else "invalid"
        ),
        "interpretation": (
            "Use the larger observed cost. A positive penalty above 20% must be disclosed and combined with the explicit horizontal-efficiency discount when no further repeat is requested."
        ),
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
