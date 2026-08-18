#!/usr/bin/env python3
"""Calculate the no-HA base capacity for the defense-60-v1 mixed model."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path


SOCIAL_FRONTEND = {"friend-list", "gen-share", "search-user", "list-friend-requests"}
CROSS_AUTH = {"enter-friend", "water-cross", "weed-cross", "pest-cross", "steal"}


def load(path: str) -> dict:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def ceil_cpu(qps: float, milliseconds: float, cpu_per_replica: float, utilization: float) -> int:
    return math.ceil(qps * milliseconds / (1000 * cpu_per_replica * utilization))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--demand", action="append", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--stable-qps", type=float, default=10000)
    parser.add_argument("--compute-utilization", type=float, default=0.70)
    parser.add_argument("--connection-utilization", type=float, default=0.65)
    parser.add_argument("--gateway-connections", type=int, default=15000)
    parser.add_argument("--gateway-handshake-qps", type=float, default=9670)
    parser.add_argument("--reconnect-ratio", type=float, default=0.10)
    parser.add_argument("--actor-max-resident", type=int, default=20000)
    parser.add_argument("--actor-ttl-seconds", type=float, default=120)
    parser.add_argument("--offline-friend-share", type=float, default=0.50)
    parser.add_argument("--social-stable-qps", type=float, default=20000)
    parser.add_argument("--mysql-stable-statements", type=float, default=20000)
    parser.add_argument("--mysql-auth-statements", type=float, default=2)
    parser.add_argument("--redis-stable-commands", type=float, default=363438.11)
    parser.add_argument("--redis-connection-commands-per-second", type=float, default=0.10)
    args = parser.parse_args()

    model = load(args.model)
    demands = [load(path) for path in args.demand]
    population = model["population"]
    business_qps = float(population["peak_business_qps"])
    pcu = float(population["peak_concurrent_users"])
    arrivals = float(population["peak_session_arrivals_per_second"])
    operation_qps = {
        operation["name"]: float(operation["reference_qps"])
        for operation in model["operations"]
        if operation.get("enabled", True)
    }
    cpu = {
        service: max(float(demand["cpu_ms_per_request"][service]) for demand in demands)
        for service in ("gateway", "farm", "social", "mysql", "redis")
    }
    mysql_per_request = max(float(demand["mysql_operations_per_request"]) for demand in demands)
    redis_per_request = max(float(demand["redis_operations_per_request"]) for demand in demands)
    mysql_cpu_per_statement = max(
        float(demand["cpu_ms_per_request"]["mysql"])
        / float(demand["mysql_operations_per_request"])
        for demand in demands
    )
    redis_cpu_per_command = max(
        float(demand["cpu_ms_per_request"]["redis"])
        / float(demand["redis_operations_per_request"])
        for demand in demands
    )
    u = args.compute_utilization

    handshake_qps = arrivals * (1 + args.reconnect_ratio)
    gateway = {
        "by_connections": math.ceil(pcu / (args.gateway_connections * args.connection_utilization)),
        "by_business_cpu": ceil_cpu(business_qps, cpu["gateway"], 1, u),
        "by_handshake": math.ceil(handshake_qps / (args.gateway_handshake_qps * u)),
    }
    gateway["base"] = max(gateway.values())

    friend_actor_qps = operation_qps["enter-friend"]
    resident_actors = (
        pcu
        + arrivals * args.actor_ttl_seconds
        + friend_actor_qps * args.actor_ttl_seconds * args.offline_friend_share
    )
    farm = {
        "estimated_resident_actors": round(resident_actors),
        "by_resident_actors": math.ceil(
            resident_actors / (args.actor_max_resident * u)
        ),
        "by_full_mix_qps": math.ceil(business_qps / (args.stable_qps * u)),
        "by_business_cpu": ceil_cpu(business_qps, cpu["farm"], 1, u),
    }
    farm["base"] = max(
        farm["by_resident_actors"], farm["by_full_mix_qps"], farm["by_business_cpu"]
    )

    social_calls = sum(operation_qps.get(name, 0) for name in SOCIAL_FRONTEND | CROSS_AUTH)
    social = {
        "peak_calls_per_second": round(social_calls, 3),
        "by_call_qps": math.ceil(social_calls / (args.social_stable_qps * u)),
        "by_business_cpu": ceil_cpu(business_qps, cpu["social"], 1, u),
    }
    social["base"] = max(social["by_call_qps"], social["by_business_cpu"])

    mysql_business = business_qps * mysql_per_request
    mysql_auth = arrivals * args.mysql_auth_statements
    mysql_total = mysql_business + mysql_auth
    mysql_cpu_cores = (
        business_qps * cpu["mysql"] + mysql_auth * mysql_cpu_per_statement
    ) / 1000
    mysql = {
        "peak_statements_per_second": round(mysql_total, 3),
        "by_statement_qps": math.ceil(mysql_total / (args.mysql_stable_statements * u)),
        "by_cpu": math.ceil(mysql_cpu_cores / (2 * u)),
    }
    mysql["base_primary_capacity_shards"] = max(mysql["by_statement_qps"], mysql["by_cpu"])

    redis_business = business_qps * redis_per_request
    redis_connections = pcu * args.redis_connection_commands_per_second
    redis_total = redis_business + redis_connections
    redis_cpu_cores = (
        business_qps * cpu["redis"] + redis_connections * redis_cpu_per_command
    ) / 1000
    redis = {
        "peak_commands_per_second": round(redis_total, 3),
        "by_command_qps": math.ceil(redis_total / (args.redis_stable_commands * u)),
        "by_cpu": math.ceil(redis_cpu_cores / u),
    }
    redis["base_primary_capacity_shards"] = max(redis["by_command_qps"], redis["by_cpu"])

    output = {
        "model": model["name"],
        "scope": "base capacity only; no HA, replicas, AZ or failure-domain allowance",
        "inputs": {
            "business_qps": business_qps,
            "pcu": pcu,
            "session_arrivals_per_second": arrivals,
            "stable_mixed_qps": args.stable_qps,
            "compute_utilization": u,
            "connection_utilization": args.connection_utilization,
            "max_cpu_ms_per_business_request": cpu,
            "mysql_operations_per_business_request": mysql_per_request,
            "redis_operations_per_business_request": redis_per_request,
            "demand_sources": [str(Path(path)) for path in args.demand],
        },
        "capacity": {
            "full_tested_topology_units_by_qps": math.ceil(business_qps / (args.stable_qps * u)),
            "gateway": gateway,
            "farm": farm,
            "social": social,
            "mysql": mysql,
            "redis": redis,
        },
        "deployment": {
            "gateway": {"instances": gateway["base"], "per_instance": "1 CPU / 1 GiB"},
            "farm": {"instances": farm["base"], "per_instance": "1 CPU / 2 GiB"},
            "social": {"instances": social["base"], "per_instance": "1 CPU / 1 GiB"},
            "mysql_primary_capacity_shards": {
                "instances": mysql["base_primary_capacity_shards"],
                "per_instance": "2 CPU / 4 GiB",
            },
            "redis_primary_capacity_shards": {
                "instances": redis["base_primary_capacity_shards"],
                "per_instance": "1 CPU / 4 GiB",
            },
        },
        "limitations": [
            "MySQL and Redis counts are capacity-equivalent primary shards; horizontal scaling requires UID-aware sharding.",
            "Redis memory capacity cannot be closed from a 15,000-account fixture and must be recalculated from production key cardinality and value sizes.",
            "The 10,000 QPS stable point is based on one final 30-second validation; repeat it and run longer endurance tests before production procurement.",
        ],
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
