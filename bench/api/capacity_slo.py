#!/usr/bin/env python3
"""按行为模型逐接口判定混合压测结果，拒绝用整体延迟掩盖失败接口。"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def ratio(numerator: int | float, denominator: int | float) -> float:
    return float(numerator) / float(denominator) if denominator else 0.0


def validate_model(model: dict[str, Any]) -> dict[str, Any]:
    operations = [item for item in model["operations"] if item.get("enabled", True)]
    names = [str(item["name"]) for item in operations]
    request_total = sum(float(item["requests_per_session"]) for item in operations)
    weight_total = sum(float(item["weight"]) for item in operations)
    reference_total = sum(float(item["reference_qps"]) for item in operations)
    expected_requests = float(model["population"]["business_requests_per_session"])
    expected_qps = float(model["population"]["peak_business_qps"])
    errors: list[str] = []
    if len(names) != len(set(names)):
        errors.append("enabled operation names are not unique")
    if len(names) != 31:
        errors.append(f"enabled operation count is {len(names)}, want 31")
    if abs(request_total - expected_requests) > 1e-6:
        errors.append(f"operation requests/session sum is {request_total}, want {expected_requests}")
    if abs(weight_total - 1.0) > 1e-9:
        errors.append(f"operation weight sum is {weight_total}, want 1")
    if abs(reference_total - expected_qps) > max(1e-3, expected_qps * 1e-8):
        errors.append(f"operation reference QPS sum is {reference_total}, want {expected_qps}")
    return {
        "pass": not errors,
        "errors": errors,
        "enabled_operations": len(names),
        "requests_per_session_sum": request_total,
        "weight_sum": weight_total,
        "reference_qps_sum": reference_total,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", required=True)
    parser.add_argument("--result", action="append", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument(
        "--screening",
        action="store_true",
        help="阶梯筛选只判流量、错误和延迟，不要求每接口累计10000样本",
    )
    parser.add_argument(
        "--sharded",
        action="store_true",
        help="results are concurrent load-generator shards; report the sum of target QPS",
    )
    parser.add_argument(
        "--exclude-operations",
        default="",
        help="comma-separated operations intentionally omitted by a module-isolation run",
    )
    parser.add_argument(
        "--extra-operation",
        action="append",
        default=[],
        help="derived module operation not present in the public behavior model",
    )
    parser.add_argument(
        "--only-operations",
        default="",
        help="comma-separated operation set expected in this module-isolation run",
    )
    args = parser.parse_args()

    model = load(args.model)
    model_check = validate_model(model)
    if not model_check["pass"]:
        raise ValueError("invalid behavior model: " + "; ".join(model_check["errors"]))

    results = [load(path) for path in args.result]
    operations = {
        str(item["name"]): item
        for item in model["operations"]
        if item.get("enabled", True)
    }
    for name in args.extra_operation:
        if name in operations:
            raise ValueError(f"extra operation already exists in model: {name}")
        operations[name] = {"name": name, "weight": 0.0}
    excluded_operations = {
        name.strip()
        for name in args.exclude_operations.split(",")
        if name.strip()
    }
    only_operations = {
        name.strip()
        for name in args.only_operations.split(",")
        if name.strip()
    }
    unknown_only_operations = sorted(only_operations - set(operations))
    if unknown_only_operations:
        raise ValueError(
            "unknown selected operations: " + ", ".join(unknown_only_operations)
        )
    if only_operations:
        operations = {
            name: operation
            for name, operation in operations.items()
            if name in only_operations
        }
    unknown_exclusions = sorted(excluded_operations - set(operations))
    if unknown_exclusions:
        raise ValueError(
            "unknown excluded operations: " + ", ".join(unknown_exclusions)
        )
    operations = {
        name: operation
        for name, operation in operations.items()
        if name not in excluded_operations
    }
    slo = model["slo"]
    delivery_min = float(slo["minimum_target_delivery_ratio"])
    error_max = float(slo["maximum_technical_error_rate"])
    p90_max = float(slo["p90_milliseconds"])
    p99_max = float(slo["p99_milliseconds"])
    sample_min = 0 if args.screening else int(slo["minimum_samples_per_operation"])
    drain_max = int(slo["maximum_client_drain_milliseconds"])

    per_shard_target_qps_values = [int(item["target_qps"]) for item in results]
    target_qps_values = set(per_shard_target_qps_values)
    per_shard_target_qps = (
        per_shard_target_qps_values[0]
        if len(target_qps_values) == 1
        else per_shard_target_qps_values
    )
    target_qps = (
        sum(per_shard_target_qps_values)
        if args.sharded
        else per_shard_target_qps_values[0]
    )

    aggregate: dict[str, dict[str, Any]] = {
        name: {
            "sent": 0,
            "succeeded": 0,
            "failed": 0,
            "worst_p90_ms": 0.0,
            "worst_p99_ms": 0.0,
            "rounds_passed": 0,
            "rounds": [],
        }
        for name in operations
    }
    round_rows: list[dict[str, Any]] = []
    for source, result in zip(args.result, results):
        missing = sorted(set(operations) - set(result.get("steps", {})))
        unexpected = sorted(set(result.get("steps", {})) - set(operations))
        completed = int(result.get("succeeded", 0)) + int(result.get("failed", 0))
        planned = (
            int(result["target_qps"])
            * float(result["measurement_millis"])
            / 1000
        )
        delivery = ratio(result.get("succeeded", 0), planned)
        error_rate = ratio(result.get("failed", 0), completed)
        resident_actor_refresh_failures = int(
            result.get("resident_actor_refresh_failures", 0)
        )
        connection_keepalive_failures = int(
            result.get("connection_keepalive_failures", 0)
        )
        interface_failures: list[str] = []
        for name, operation in operations.items():
            step = result.get("steps", {}).get(name)
            if step is None:
                continue
            sent = int(step.get("sent", 0))
            succeeded = int(step.get("succeeded", 0))
            failed = int(step.get("failed", 0))
            step_error = ratio(failed, sent)
            p90 = float(step.get("p90_ms", 0))
            p99 = float(step.get("p99_ms", 0))
            expected_weight = float(operation["weight"])
            actual_weight = ratio(sent, int(result.get("sent", 0)))
            step_pass = step_error <= error_max and p90 < p90_max and p99 < p99_max
            if not step_pass:
                interface_failures.append(name)
            row = aggregate[name]
            row["sent"] += sent
            row["succeeded"] += succeeded
            row["failed"] += failed
            row["worst_p90_ms"] = max(row["worst_p90_ms"], p90)
            row["worst_p99_ms"] = max(row["worst_p99_ms"], p99)
            row["rounds_passed"] += int(step_pass)
            row["rounds"].append(
                {
                    "source": str(Path(source).resolve()),
                    "sent": sent,
                    "error_rate": step_error,
                    "p90_ms": p90,
                    "p99_ms": p99,
                    "expected_weight": expected_weight,
                    "actual_weight": actual_weight,
                    "pass": step_pass,
                }
            )
        round_pass = (
            not missing
            and not unexpected
            and delivery >= delivery_min
            and error_rate <= error_max
            and not interface_failures
            and not bool(result.get("timed_out", False))
            and resident_actor_refresh_failures == 0
            and connection_keepalive_failures == 0
        )
        round_rows.append(
            {
                "source": str(Path(source).resolve()),
                "target_qps": int(result["target_qps"]),
                "delivery_ratio": delivery,
                "technical_error_rate": error_rate,
                "client_drain_milliseconds": int(result.get("drain_millis", 0)),
                "client_drain_within_diagnostic_limit": (
                    int(result.get("drain_millis", 0)) <= drain_max
                ),
                "missing_operations": missing,
                "unexpected_operations": unexpected,
                "failed_operations": interface_failures,
                "resident_actor_refresh_failures": resident_actor_refresh_failures,
                "connection_keepalive_failures": connection_keepalive_failures,
                "pass": round_pass,
            }
        )

    interface_rows: dict[str, dict[str, Any]] = {}
    for name, row in aggregate.items():
        error_rate = ratio(row["failed"], row["sent"])
        row_pass = (
            row["rounds_passed"] == len(results)
            and row["sent"] >= sample_min
            and error_rate <= error_max
            and row["worst_p90_ms"] < p90_max
            and row["worst_p99_ms"] < p99_max
        )
        interface_rows[name] = {
            **row,
            "technical_error_rate": error_rate,
            "minimum_samples_required": sample_min,
            "pass": row_pass,
        }

    verdict = all(item["pass"] for item in round_rows) and all(
        item["pass"] for item in interface_rows.values()
    )
    output = {
        "model": model["name"],
        "model_validation": model_check,
        "mode": "screening" if args.screening else "candidate-certification",
        "target_qps": target_qps,
        "per_shard_target_qps": per_shard_target_qps if args.sharded else None,
        "shards": len(results) if args.sharded else 1,
        "excluded_operations": sorted(excluded_operations),
        "extra_operations": sorted(args.extra_operation),
        "only_operations": sorted(only_operations),
        "slo": slo,
        "rounds": round_rows,
        "operations": interface_rows,
        "verdict": "pass" if verdict else "fail",
        "limitations": [
            "Latency percentiles are taken from successful responses; technical errors are judged separately.",
            "Client drain time is diagnostic only; making it a pass/fail gate would add an undeclared maximum-latency SLO on top of P90/P99.",
            "Journal/Projection sustainability is not inferred from client JSON and must be supplied by the Prometheus window report.",
        ],
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
