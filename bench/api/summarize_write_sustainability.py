#!/usr/bin/env python3
"""比较完整混合负载、写链路校准和长稳轮次，并给出可持续性判定。"""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


def load(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def finite(value: Any) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return number if math.isfinite(number) else None


def counter(prom: dict[str, Any], window: str, name: str, field: str = "per_second") -> float | None:
    try:
        return finite(prom["windows"][window]["system_metrics"]["counters"][name][field])
    except (KeyError, TypeError):
        return None


def gauge(prom: dict[str, Any], window: str, name: str, field: str) -> float | None:
    try:
        return finite(prom["windows"][window]["system_metrics"]["gauges"][name][field])
    except (KeyError, TypeError):
        return None


def relative_difference(left: float | None, right: float | None) -> float | None:
    if left is None or right is None or right == 0:
        return None
    return abs(left - right) / abs(right)


def summarize(run_dir: Path) -> dict[str, Any]:
    client = load(run_dir / "client.json")
    prom = load(run_dir / "prometheus-windows.json")
    context = load(run_dir / "window-context.json")
    qps = finite(prom.get("derived", {}).get("successful_qps")) or 0.0
    measurement_end_ms = int(client["measurement_start_unix_ms"]) + int(client["measurement_millis"])
    drain_seconds = max(
        (int(context["journal_idle_unix_ms"]) - measurement_end_ms) / 1000,
        0.0,
    )
    append_rate = counter(prom, "C_load", "farm_journal_append_records")
    projection_rate = counter(prom, "C_load", "farm_projection_records")
    cd_append = counter(prom, "CD_complete_accounting", "farm_journal_append_records", "increase")
    cd_projection = counter(prom, "CD_complete_accounting", "farm_projection_records", "increase")
    operation_rows = list(client.get("steps", {}).values())
    output: dict[str, Any] = {
        "run_id": run_dir.name,
        "run_dir": str(run_dir.resolve()),
        "target_qps": int(client["target_qps"]),
        "successful_qps": qps,
        "duration_seconds": float(client["measurement_millis"]) / 1000,
        "sent": int(client["sent"]),
        "failed": int(client["failed"]),
        "technical_error_rate": int(client["failed"]) / max(int(client["sent"]), 1),
        "worst_operation_p90_ms": max((finite(item.get("p90_ms")) or 0.0 for item in operation_rows), default=0.0),
        "worst_operation_p99_ms": max((finite(item.get("p99_ms")) or 0.0 for item in operation_rows), default=0.0),
        "journal_drain_seconds": drain_seconds,
        "journal_append_records_per_second": append_rate,
        "journal_append_records_per_request": append_rate / qps if append_rate is not None and qps > 0 else None,
        "projection_records_per_second_during_load": projection_rate,
        "projection_to_append_ratio_complete_window": (
            cd_projection / cd_append
            if cd_projection is not None and cd_append is not None and cd_append > 0
            else None
        ),
        "mysql_questions_per_second": counter(prom, "C_load", "mysql_questions"),
        "redis_commands_per_second": counter(prom, "C_load", "redis_commands"),
        "journal_append_errors": counter(prom, "CD_complete_accounting", "farm_journal_append_errors", "increase"),
        "projection_errors": counter(prom, "CD_complete_accounting", "farm_projection_errors", "increase"),
        "write_admission_rejected": counter(prom, "C_load", "farm_write_admission_rejected", "increase"),
        "write_pending_C_peak": gauge(prom, "C_load", "farm_write_pending", "peak"),
        "write_pending_C_last": gauge(prom, "C_load", "farm_write_pending", "last"),
        "write_pending_D_last": gauge(prom, "D_recovery", "farm_write_pending", "last"),
        "write_lag_D_last": gauge(prom, "D_recovery", "farm_write_lag", "last"),
        "complete_cpu_milliseconds_per_request": {
            service: finite(values.get("complete_cpu_milliseconds_per_successful_request"))
            for service, values in prom.get("derived", {}).get("services", {}).items()
            if service in ("gateway", "farm", "social", "mysql", "redis")
        },
    }
    return output


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--full-mix", required=True)
    parser.add_argument("--calibration", required=True)
    parser.add_argument("--soak")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    full = summarize(Path(args.full_mix))
    calibration = summarize(Path(args.calibration))
    full_write = finite(full["journal_append_records_per_request"])
    calibration_write = finite(calibration["journal_append_records_per_request"])
    calibration_error = relative_difference(calibration_write, full_write)
    calibration_pass = calibration_error is not None and calibration_error <= 0.10

    output: dict[str, Any] = {
        "method": "full 31-operation mix -> repeatable-write calibration -> 10-minute soak",
        "full_mix": full,
        "calibration": calibration,
        "calibration_write_cost_relative_difference": calibration_error,
        "calibration_verdict": "pass" if calibration_pass else "fail",
        "rules": {
            "calibration_maximum_write_cost_relative_difference": 0.10,
            "maximum_technical_error_rate": 0.001,
            "maximum_operation_p90_ms": 300.0,
            "maximum_operation_p99_ms": 500.0,
            "maximum_journal_drain_seconds": 60.0,
            "minimum_complete_projection_ratio": 0.99,
            "maximum_background_errors_or_rejections": 0.0,
        },
    }

    if args.soak:
        soak = summarize(Path(args.soak))
        soak_write = finite(soak["journal_append_records_per_request"])
        soak_calibration_error = relative_difference(soak_write, calibration_write)
        checks = {
            "calibration_pass": calibration_pass,
            "write_cost_matches_calibration": (
                soak_calibration_error is not None and soak_calibration_error <= 0.10
            ),
            "technical_error_slo": soak["technical_error_rate"] <= 0.001,
            "operation_p90_slo": soak["worst_operation_p90_ms"] < 300.0,
            "operation_p99_slo": soak["worst_operation_p99_ms"] < 500.0,
            "journal_drains_within_60s": soak["journal_drain_seconds"] <= 60.0,
            "complete_projection_catches_up": (
                finite(soak["projection_to_append_ratio_complete_window"]) is not None
                and float(soak["projection_to_append_ratio_complete_window"]) >= 0.99
            ),
            "journal_empty_after_recovery": (
                (finite(soak["write_pending_D_last"]) or 0.0) == 0.0
                and (finite(soak["write_lag_D_last"]) or 0.0) == 0.0
            ),
            "no_background_errors_or_rejections": all(
                (finite(soak[name]) or 0.0) == 0.0
                for name in (
                    "journal_append_errors",
                    "projection_errors",
                    "write_admission_rejected",
                )
            ),
        }
        output.update(
            {
                "soak": soak,
                "soak_write_cost_relative_difference_from_calibration": soak_calibration_error,
                "soak_checks": checks,
                "soak_verdict": "pass" if all(checks.values()) else "fail",
            }
        )

    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
