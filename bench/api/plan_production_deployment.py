#!/usr/bin/env python3
"""Convert 10k-derived logical resources into basic and three-AZ deployment plans."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
from typing import Any


SPECS: dict[str, dict[str, float | int | str]] = {
    "gateway": {"cpu": 4, "memory_gib": 12, "density": 100000, "density_name": "connections"},
    "farm": {"cpu": 4, "memory_gib": 8, "density": 150000, "density_name": "actors"},
    "social": {"cpu": 2, "memory_gib": 1, "density": 0, "density_name": "none"},
    "mysql": {"cpu": 64, "memory_gib": 192, "density": 0, "density_name": "vertical-primary"},
    "redis": {"cpu": 2, "memory_gib": 2, "density": 0, "density_name": "cpu-bound-shard"},
}

LOADS = {"connections": 4_000_000, "actors": 5_420_000}


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def ceil_div(value: float, divisor: float) -> int:
    return math.ceil(value / divisor)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--capacity", required=True)
    parser.add_argument("--output-json", required=True)
    parser.add_argument("--output-markdown", required=True)
    args = parser.parse_args()

    capacity = load(args.capacity)
    basic: dict[str, Any] = {}
    for service, spec in SPECS.items():
        row = capacity["services"][service]
        cpu_instances = ceil_div(float(row["planned_cpu_cores"]), float(spec["cpu"]))
        memory_instances = ceil_div(
            float(row["planned_memory_mib"]) / 1024, float(spec["memory_gib"])
        )
        density_instances = 0
        density_name = str(spec["density_name"])
        if density_name in LOADS:
            density_instances = ceil_div(LOADS[density_name], float(spec["density"]))
        instances = max(cpu_instances, memory_instances, density_instances, 1)
        # MySQL remains one vertically sized primary in the basic plan. Redis
        # must shard because one process cannot consume the extrapolated cores.
        basic[service] = {
            "instances": instances,
            "cpu_per_instance": int(spec["cpu"]),
            "memory_gib_per_instance": int(spec["memory_gib"]),
            "constraints": {
                "cpu": cpu_instances,
                "memory": memory_instances,
                "density": density_instances,
            },
            "total_cpu": instances * int(spec["cpu"]),
            "total_memory_gib": instances * int(spec["memory_gib"]),
        }

    ha: dict[str, Any] = {}
    for service in ("gateway", "farm", "social"):
        base = basic[service]
        instances = math.ceil(int(base["instances"]) * 3 / 2)
        # Verify that losing the largest AZ still leaves the basic count.
        largest_az = math.ceil(instances / 3)
        while instances - largest_az < int(base["instances"]):
            instances += 1
            largest_az = math.ceil(instances / 3)
        ha[service] = {
            **base,
            "instances": instances,
            "az_distribution": [
                math.ceil(instances / 3),
                math.ceil((instances - math.ceil(instances / 3)) / 2),
                instances - math.ceil(instances / 3) - math.ceil((instances - math.ceil(instances / 3)) / 2),
            ],
            "total_cpu": instances * int(base["cpu_per_instance"]),
            "total_memory_gib": instances * int(base["memory_gib_per_instance"]),
        }

    mysql = basic["mysql"]
    ha["mysql"] = {
        **mysql,
        "instances": 3,
        "roles": "1 primary + 2 replicas, one node per AZ",
        "az_distribution": [1, 1, 1],
        "total_cpu": 3 * int(mysql["cpu_per_instance"]),
        "total_memory_gib": 3 * int(mysql["memory_gib_per_instance"]),
    }
    redis = basic["redis"]
    redis_masters = int(redis["instances"])
    ha["redis"] = {
        **redis,
        "instances": redis_masters * 2,
        "roles": f"{redis_masters} masters + {redis_masters} cross-AZ replicas",
        "az_distribution": [redis_masters * 2 // 3] * 3,
        "total_cpu": redis_masters * 2 * int(redis["cpu_per_instance"]),
        "total_memory_gib": redis_masters * 2 * int(redis["memory_gib_per_instance"]),
    }

    def totals(plan: dict[str, Any]) -> dict[str, int]:
        return {
            "instances": sum(int(row["instances"]) for row in plan.values()),
            "cpu": sum(int(row["total_cpu"]) for row in plan.values()),
            "memory_gib": sum(int(row["total_memory_gib"]) for row in plan.values()),
        }

    output = {
        "basic": basic,
        "basic_totals": totals(basic),
        "ha": ha,
        "ha_totals": totals(ha),
        "ha_policy": "three AZ; stateless N*1.5; MySQL 1+2; Redis one cross-AZ replica per master",
    }
    Path(args.output_json).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    lines = [
        "# 10k容量实例化方案",
        "",
        "## 基础规划",
        "",
        "| 服务 | 实例数 | 单实例 | CPU约束 | 内存约束 | 密度约束 | 总CPU | 总内存GiB |",
        "| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |",
    ]
    for service, row in basic.items():
        constraints = row["constraints"]
        lines.append(
            f"| {service} | {row['instances']} | {row['cpu_per_instance']}C/{row['memory_gib_per_instance']}GiB | "
            f"{constraints['cpu']} | {constraints['memory']} | {constraints['density']} | "
            f"{row['total_cpu']} | {row['total_memory_gib']} |"
        )
    bt = output["basic_totals"]
    lines.append(f"| **合计** | **{bt['instances']}** | — | — | — | — | **{bt['cpu']}** | **{bt['memory_gib']}** |")
    lines.extend([
        "",
        "## 三可用区容灾规划",
        "",
        "| 服务 | 总实例 | AZ分布/角色 | 单实例 | 总CPU | 总内存GiB |",
        "| --- | ---: | --- | --- | ---: | ---: |",
    ])
    for service, row in ha.items():
        placement = row.get("roles") or "/".join(str(x) for x in row["az_distribution"])
        lines.append(
            f"| {service} | {row['instances']} | {placement} | "
            f"{row['cpu_per_instance']}C/{row['memory_gib_per_instance']}GiB | "
            f"{row['total_cpu']} | {row['total_memory_gib']} |"
        )
    ht = output["ha_totals"]
    lines.append(f"| **合计** | **{ht['instances']}** | — | — | **{ht['cpu']}** | **{ht['memory_gib']}** |")
    lines.append("")
    Path(args.output_markdown).write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    main()
