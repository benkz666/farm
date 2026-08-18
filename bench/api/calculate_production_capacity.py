#!/usr/bin/env python3
"""把实测资源系数代入峰值工作负载，独立计算CPU、内存和数量约束。"""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


SERVICES = ("gateway", "farm", "social", "mysql", "redis")


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def ceil_ratio(numerator: float, denominator: float) -> int:
    if denominator <= 0:
        raise ValueError(f"capacity denominator must be positive, got {denominator}")
    return math.ceil(numerator / denominator)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan", required=True)
    parser.add_argument("--cost-summary", required=True)
    parser.add_argument("--topology-comparison", required=True)
    parser.add_argument("--dataset-summary", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    plan = load(args.plan)
    costs = load(args.cost_summary)
    topology = load(args.topology_comparison)
    dataset = load(args.dataset_summary)
    workload = plan["workload"]
    policy = plan["planning"]
    specs = plan["production_instance_specs"]
    limits = plan["validated_limits"]

    qps = float(workload["peak_business_qps"])
    connections = float(workload["peak_connections"])
    actor_working_set = connections + (
        float(workload["peak_session_arrivals_per_second"])
        + float(workload["enter_friend_qps"])
        * float(workload["unique_offline_actor_activation_ratio"])
    ) * float(workload["actor_idle_seconds"])
    redis_active_accounts = actor_working_set
    registered_accounts = float(workload["registered_accounts_lower_bound"])
    mysql_bytes_per_account = float(dataset["mysql"]["data_plus_index_bytes_per_account"])
    redis_bytes_per_active = float(
        dataset["redis"]["dataset_bytes_per_active_test_account_upper_bound"]
    )
    mysql_disk_bytes = (
        registered_accounts
        * mysql_bytes_per_account
        * float(policy["mysql_disk_expansion_factor"])
    )
    redis_dataset_bytes = (
        redis_active_accounts
        * redis_bytes_per_active
        * float(policy["redis_dataset_expansion_factor"])
    )

    rows: dict[str, Any] = {}
    for service in SERVICES:
        coeff = costs["coefficients"][service]
        topology_row = topology["services"][service]
        measured_cpu_per_1000 = float(coeff["planning_cpu_per_1000_qps"])
        cross_cpu = topology_row.get("cross_topology_planning_cpu_per_1000_qps")
        cpu_per_1000 = max(
            measured_cpu_per_1000,
            float(cross_cpu) if cross_cpu is not None else measured_cpu_per_1000,
        )
        memory_per_1000 = float(coeff["planning_memory_mib_per_1000_qps"])
        fixed_cpu = float(coeff["fixed_cpu_cores_per_instance_max"] or 0.0)
        fixed_memory = float(coeff["fixed_memory_mib_per_instance_max"] or 0.0)
        # MySQL生产规格会主动预热更大的Buffer Pool；测试环境的固定工作集
        # 不能低估这部分配置相关内存，因此用Buffer Pool加2GiB进程/连接余量覆盖。
        if service == "mysql":
            fixed_memory = max(
                fixed_memory,
                (float(specs[service]["buffer_pool_gib"]) + 2.0) * 1024,
            )

        state_units = 0.0
        state_cpu_per_unit = 0.0
        state_memory_per_unit = 0.0
        if service == "gateway":
            state_units = connections
        elif service == "farm":
            state_units = actor_working_set
        elif service == "redis":
            state_units = connections
        if state_units:
            state_cpu_per_unit = float(coeff["state_cpu_cores_per_unit_max"] or 0.0)
            state_memory_per_unit = float(coeff["state_memory_mib_per_unit_max"] or 0.0)

        variable_cpu = qps / 1000 * cpu_per_1000 + state_units * state_cpu_per_unit
        variable_memory = qps / 1000 * memory_per_1000 + state_units * state_memory_per_unit
        if service == "redis":
            variable_memory += redis_dataset_bytes / 1048576

        gamma = float(policy["horizontal_efficiency"][service])
        cpu_per_instance = float(specs[service]["cpu_cores"])
        memory_per_instance_mib = float(specs[service]["memory_gib"]) * 1024
        usable_variable_cpu = gamma * (
            cpu_per_instance * float(policy["cpu_utilization"]) - fixed_cpu
        )
        usable_variable_memory = gamma * (
            memory_per_instance_mib * float(policy["memory_utilization"]) - fixed_memory
        )
        cpu_instances = ceil_ratio(variable_cpu, usable_variable_cpu)
        memory_instances = ceil_ratio(variable_memory, usable_variable_memory)
        tested_density = float(limits["single_round_stable_mixed_qps"]) / float(
            limits["candidate_topology"][service]
        )
        density_instances = ceil_ratio(qps, tested_density)

        constraints: dict[str, int] = {
            "cpu": cpu_instances,
            "memory": memory_instances,
            "validated_business_density": density_instances,
        }
        if service == "gateway":
            constraints["connections"] = ceil_ratio(
                connections,
                float(limits["gateway_connections_per_instance_lower_bound"])
                * float(policy["quantity_utilization"])
                * gamma,
            )
        elif service == "farm":
            constraints["actors"] = ceil_ratio(
                actor_working_set,
                float(limits["farm_actors_per_instance_configured_limit"])
                * float(policy["quantity_utilization"])
                * gamma,
            )
        elif service == "mysql":
            constraints["disk"] = ceil_ratio(
                mysql_disk_bytes / 1073741824,
                float(specs[service]["usable_disk_gib"])
                * float(policy["quantity_utilization"]),
            )
        elif service == "redis":
            constraints["dataset"] = ceil_ratio(
                redis_dataset_bytes / 1073741824,
                float(specs[service]["maxmemory_gib"]) * gamma,
            )

        final_instances = max(constraints.values())
        binding = sorted(name for name, value in constraints.items() if value == final_instances)
        rows[service] = {
            "measured_cpu_cores_per_1000_qps": measured_cpu_per_1000,
            "cross_topology_cpu_cores_per_1000_qps": cross_cpu,
            "planning_cpu_cores_per_1000_qps": cpu_per_1000,
            "planning_memory_mib_per_1000_qps": memory_per_1000,
            "fixed_cpu_cores_per_instance": fixed_cpu,
            "fixed_memory_mib_per_instance": fixed_memory,
            "state_units": state_units,
            "state_cpu_cores_per_unit": state_cpu_per_unit,
            "state_memory_mib_per_unit": state_memory_per_unit,
            "variable_cpu_cores": variable_cpu,
            "variable_memory_mib": variable_memory,
            "horizontal_efficiency": gamma,
            "usable_variable_cpu_cores_per_instance": usable_variable_cpu,
            "usable_variable_memory_mib_per_instance": usable_variable_memory,
            "validated_business_qps_density_per_instance": tested_density,
            "constraints": constraints,
            "baseline_instances_or_primary_shards": final_instances,
            "binding_constraints": binding,
            "instance_spec": specs[service],
            "requested_cpu_cores": final_instances * cpu_per_instance,
            "requested_memory_gib": final_instances * float(specs[service]["memory_gib"]),
        }

    base_cpu = sum(float(row["requested_cpu_cores"]) for row in rows.values())
    base_memory = sum(float(row["requested_memory_gib"]) for row in rows.values())
    dr = plan["disaster_recovery"]
    recovery_rows: dict[str, Any] = {}
    for service, row in rows.items():
        base_instances = int(row["baseline_instances_or_primary_shards"])
        if service in ("mysql", "redis"):
            copies = int(dr[f"{service}_copies_per_primary_shard"])
            recovery_instances = base_instances * copies
            interpretation = f"{base_instances} primary shards x {copies} copies"
        else:
            recovery_instances = math.ceil(
                base_instances * float(dr["stateless_and_actor_capacity_multiplier"])
            )
            interpretation = "three-zone placement with one-zone-loss capacity"
        recovery_rows[service] = {
            "baseline_instances_or_primary_shards": base_instances,
            "disaster_recovery_instances": recovery_instances,
            "interpretation": interpretation,
            "requested_cpu_cores": recovery_instances
            * float(specs[service]["cpu_cores"]),
            "requested_memory_gib": recovery_instances
            * float(specs[service]["memory_gib"]),
        }

    output = {
        "method": "independent CPU, memory, connection/Actor, validated-density and data constraints",
        "sources": {
            "plan": str(Path(args.plan).resolve()),
            "cost_summary": str(Path(args.cost_summary).resolve()),
            "topology_comparison": str(Path(args.topology_comparison).resolve()),
            "dataset_summary": str(Path(args.dataset_summary).resolve()),
        },
        "workload": {
            "peak_business_qps": qps,
            "peak_connections": connections,
            "farm_actor_working_set": actor_working_set,
            "registered_accounts_lower_bound": registered_accounts,
            "redis_active_account_upper_bound": redis_active_accounts,
            "mysql_disk_bytes_with_expansion": mysql_disk_bytes,
            "redis_dataset_bytes_with_expansion": redis_dataset_bytes,
        },
        "baseline": {
            "services": rows,
            "requested_cpu_cores": base_cpu,
            "requested_memory_gib": base_memory,
            "note": "MySQL/Redis counts are writable primary shards; no disaster-recovery replicas are included.",
        },
        "disaster_recovery": {
            "failure_model": dr["failure_model"],
            "services": recovery_rows,
            "requested_cpu_cores": sum(
                float(row["requested_cpu_cores"]) for row in recovery_rows.values()
            ),
            "requested_memory_gib": sum(
                float(row["requested_memory_gib"]) for row in recovery_rows.values()
            ),
        },
        "sensitivity": {
            "mysql_disk_bytes_per_additional_registered_account_before_expansion": mysql_bytes_per_account,
            "redis_dataset_bytes_per_additional_active_account_before_expansion": redis_bytes_per_active,
        },
        "limitations": plan["limitations"],
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
