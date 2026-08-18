#!/usr/bin/env python3
"""汇总多个A/B/C/D成本档，拟合服务资源系数并判定档位是否可用。"""

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


def maximum(values: list[float | None]) -> float | None:
    selected = [value for value in values if value is not None]
    return max(selected) if selected else None


def service_resource(baseline: dict[str, Any], service: str, name: str) -> float:
    return float(baseline["resources"][service][name])


def gauge_last(prom: dict[str, Any], window: str, name: str) -> float | None:
    try:
        return finite(prom["windows"][window]["system_metrics"]["gauges"][name]["last"])
    except (KeyError, TypeError):
        return None


def state_units(prom: dict[str, Any], service: str) -> float | None:
    if service == "farm":
        return gauge_last(prom, "B_state", "farm_actor_resident")
    if service in ("gateway", "redis"):
        return gauge_last(prom, "B_state", "gateway_ws_connections")
    return None


def per_pod_peak(prom: dict[str, Any], service: str, metric_path: list[str]) -> tuple[float | None, str | None]:
    # 正式负载窗口 C 是首选。若短窗口恰遇 cAdvisor 抓取缺口，则用覆盖
    # C 和排空期 D 的完整核算窗口回退；后者只可能更保守，不会低估峰值。
    for window in ("C_load", "CD_complete_accounting"):
        try:
            current: Any = prom["windows"][window]["services"][service]
            for part in metric_path:
                current = current[part]
            value = maximum([finite(item.get("peak")) for item in current.values()])
            if value is not None:
                return value, window
        except (KeyError, TypeError, AttributeError):
            continue
    return None, None


def throttling_window(prom: dict[str, Any], service: str) -> tuple[dict[str, Any], str | None]:
    for window in ("C_load", "CD_complete_accounting"):
        try:
            value = prom["windows"][window]["services"][service].get("throttling")
            if value:
                return value, window
        except (KeyError, TypeError, AttributeError):
            continue
    return {}, None


def through_origin(points: list[tuple[float, float]]) -> dict[str, Any]:
    denominator = sum(x * x for x, _ in points)
    slope = sum(x * y for x, y in points) / denominator if denominator else 0.0
    rows: list[dict[str, float]] = []
    deviations: list[float] = []
    for x, actual in points:
        predicted = slope * x
        deviation = abs(actual - predicted) / actual if actual > 0 else 0.0
        deviations.append(deviation)
        rows.append(
            {
                "qps": x,
                "actual": actual,
                "predicted": predicted,
                "relative_deviation": deviation,
            }
        )
    return {
        "slope_per_qps": slope,
        "slope_per_1000_qps": slope * 1000,
        "maximum_relative_deviation": max(deviations, default=0.0),
        "points": rows,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", required=True)
    parser.add_argument("--run-dir", action="append", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    baseline = load(Path(args.baseline))
    topology = baseline["topology"]
    runs: list[dict[str, Any]] = []

    for raw_dir in args.run_dir:
        run_dir = Path(raw_dir)
        client = load(run_dir / "client.json")
        prom = load(run_dir / "prometheus-windows.json")
        slo_path = run_dir / "slo.json"
        slo = load(slo_path) if slo_path.exists() else None
        actual_qps = float(prom["derived"]["successful_qps"])
        measurement_seconds = float(client["measurement_millis"]) / 1000
        row: dict[str, Any] = {
            "run_id": run_dir.name,
            "run_dir": str(run_dir.resolve()),
            "target_qps": int(client["target_qps"]),
            "successful_qps": actual_qps,
            "sent": int(client["sent"]),
            "succeeded": int(client["succeeded"]),
            "failed": int(client["failed"]),
            "p90_ms": float(client["p90_ms"]),
            "p99_ms": float(client["p99_ms"]),
            "slo_verdict": slo.get("verdict") if slo else "not-run",
            "services": {},
        }
        valid = slo is None or slo.get("verdict") == "pass"
        invalid_reasons: list[str] = []
        for service in SERVICES:
            derived = prom["derived"]["services"][service]
            cpu_limit = service_resource(baseline, service, "cpu_per_instance")
            memory_limit = service_resource(baseline, service, "memory_gib_per_instance") * 1024
            cpu_peak, cpu_peak_window = per_pod_peak(
                prom, service, ["cpu", "per_pod_smoothed_30s"]
            )
            memory_peak, memory_peak_window = per_pod_peak(
                prom, service, ["memory_working_set_mib", "per_pod"]
            )
            throttle, throttle_window = throttling_window(prom, service)
            throttle_core_ratio = finite(throttle.get("throttled_core_ratio")) or 0.0
            throttle_period_ratio = finite(throttle.get("throttled_period_ratio")) or 0.0
            cpu_ratio = cpu_peak / cpu_limit if cpu_peak is not None else None
            memory_ratio = memory_peak / memory_limit if memory_peak is not None else None
            complete_seconds = finite(derived.get("complete_business_cpu_seconds"))
            equivalent_cpu = (
                complete_seconds / measurement_seconds
                if complete_seconds is not None and measurement_seconds > 0
                else None
            )
            units = state_units(prom, service)
            fixed_cpu = finite(derived.get("fixed_cpu_cores_A"))
            state_cpu = finite(derived.get("state_cpu_cores_B_minus_A"))
            fixed_memory = finite(derived.get("fixed_memory_mib_A"))
            state_memory = finite(derived.get("state_memory_mib_B_minus_A"))
            service_row = {
                "replicas": int(topology[service]),
                "cpu_peak_per_pod_cores": cpu_peak,
                "cpu_peak_per_pod_ratio": cpu_ratio,
                "cpu_peak_source_window": cpu_peak_window,
                "memory_peak_per_pod_mib": memory_peak,
                "memory_peak_per_pod_ratio": memory_ratio,
                "memory_peak_source_window": memory_peak_window,
                "throttled_core_ratio": throttle_core_ratio,
                "throttled_period_ratio": throttle_period_ratio,
                "throttling_source_window": throttle_window,
                "fixed_cpu_cores_per_instance": (
                    fixed_cpu / float(topology[service]) if fixed_cpu is not None else None
                ),
                "fixed_memory_mib_per_instance": (
                    fixed_memory / float(topology[service]) if fixed_memory is not None else None
                ),
                "state_units_B": units,
                "state_cpu_cores_per_unit": (
                    max(state_cpu, 0.0) / units
                    if state_cpu is not None and units is not None and units > 0
                    else None
                ),
                "state_memory_mib_per_unit": (
                    max(state_memory, 0.0) / units
                    if state_memory is not None and units is not None and units > 0
                    else None
                ),
                "equivalent_complete_business_cpu_cores": equivalent_cpu,
                "load_memory_mib": finite(derived.get("load_memory_mib_C_minus_B")),
            }
            row["services"][service] = service_row
            if cpu_ratio is None or cpu_ratio >= 0.60:
                invalid_reasons.append(f"{service}: cpu peak ratio {cpu_ratio}")
            if memory_ratio is None or memory_ratio >= 0.70:
                invalid_reasons.append(f"{service}: memory peak ratio {memory_ratio}")
            if throttle_core_ratio >= 0.005 or throttle_period_ratio >= 0.05:
                invalid_reasons.append(
                    f"{service}: throttling core={throttle_core_ratio} period={throttle_period_ratio}"
                )
        if not valid:
            invalid_reasons.append("per-operation P90/P99/error SLO failed")
        row["valid_cost_point"] = valid and not invalid_reasons
        row["invalid_reasons"] = invalid_reasons
        runs.append(row)

    valid_runs = [item for item in runs if item["valid_cost_point"]]
    coefficients: dict[str, Any] = {}
    for service in SERVICES:
        cpu_points: list[tuple[float, float]] = []
        memory_points: list[tuple[float, float]] = []
        fixed_cpu_values: list[float] = []
        fixed_memory_values: list[float] = []
        state_cpu_values: list[float] = []
        state_memory_values: list[float] = []
        for run in valid_runs:
            service_row = run["services"][service]
            qps = float(run["successful_qps"])
            cpu = finite(service_row["equivalent_complete_business_cpu_cores"])
            memory = finite(service_row["load_memory_mib"])
            if cpu is not None:
                cpu_points.append((qps, cpu))
            if memory is not None:
                memory_points.append((qps, memory))
            for output, key in (
                (fixed_cpu_values, "fixed_cpu_cores_per_instance"),
                (fixed_memory_values, "fixed_memory_mib_per_instance"),
                (state_cpu_values, "state_cpu_cores_per_unit"),
                (state_memory_values, "state_memory_mib_per_unit"),
            ):
                value = finite(service_row.get(key))
                if value is not None:
                    output.append(value)
        cpu_fit = through_origin(cpu_points)
        memory_fit = through_origin(memory_points)
        maximum_observed_cpu_per_1000 = max(
            (cpu * 1000 / qps for qps, cpu in cpu_points if qps > 0),
            default=None,
        )
        maximum_observed_memory_per_1000 = max(
            (memory * 1000 / qps for qps, memory in memory_points if qps > 0),
            default=None,
        )
        cpu_linear_enough = cpu_fit["maximum_relative_deviation"] <= 0.20
        planning_cpu_per_1000 = (
            cpu_fit["slope_per_1000_qps"]
            if cpu_linear_enough
            else maximum_observed_cpu_per_1000
        )
        coefficients[service] = {
            "fixed_cpu_cores_per_instance_max": max(fixed_cpu_values, default=None),
            "fixed_memory_mib_per_instance_max": max(fixed_memory_values, default=None),
            "state_cpu_cores_per_unit_max": max(state_cpu_values, default=None),
            "state_memory_mib_per_unit_max": max(state_memory_values, default=None),
            "cpu_fit": cpu_fit,
            "maximum_observed_cpu_per_1000_qps": maximum_observed_cpu_per_1000,
            "cpu_linear_enough": cpu_linear_enough,
            "planning_cpu_per_1000_qps": planning_cpu_per_1000,
            "planning_cpu_method": (
                "through-origin fit"
                if cpu_linear_enough
                else "maximum observed unit cost because fit deviation exceeds 20%"
            ),
            "memory_fit": memory_fit,
            "maximum_observed_load_memory_mib": max(memory_points, key=lambda point: point[1])[1]
            if memory_points
            else None,
            "maximum_observed_load_memory_mib_per_1000_qps": maximum_observed_memory_per_1000,
            "planning_memory_mib_per_1000_qps": (
                memory_fit["slope_per_1000_qps"]
                if memory_fit["maximum_relative_deviation"] <= 0.20
                else maximum_observed_memory_per_1000
            ),
            "planning_memory_method": (
                "through-origin fit"
                if memory_fit["maximum_relative_deviation"] <= 0.20
                else "maximum observed unit cost because fit deviation exceeds 20%"
            ),
            "memory_linear_enough": memory_fit["maximum_relative_deviation"] <= 0.20,
        }

    output = {
        "method": "A/B/C/D aggregate-resource through-origin fit",
        "baseline": str(Path(args.baseline).resolve()),
        "valid_cost_points": len(valid_runs),
        "runs": runs,
        "coefficients": coefficients,
        "rules": {
            "per_operation_slo": "P90 < 300ms, P99 < 500ms, technical error <= 0.1%",
            "maximum_cpu_ratio_per_pod": 0.60,
            "maximum_memory_ratio_per_pod": 0.70,
            "maximum_throttled_core_ratio": 0.005,
            "maximum_throttled_period_ratio": 0.05,
            "cpu_fit_maximum_relative_deviation": 0.20,
            "memory_fit_maximum_relative_deviation": 0.20,
        },
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
