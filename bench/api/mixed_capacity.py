#!/usr/bin/env python3
"""根据用户行为模型和实测摘要计算 3000 万 DAU 的容量单元。"""

import argparse
import json
import math


def load(path):
    with open(path, encoding="utf-8") as source:
        return json.load(source)


def round_up(value, multiple):
    return int(math.ceil(value / multiple) * multiple)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--benchmarks", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    model = load(args.model)
    measured = load(args.benchmarks)
    population = model["population"]
    startup = model["startup_and_connection_load"]
    derived = model["derived_reference_business_qps"]
    policy = measured["capacity_policy"]
    topology = measured["environment"]["topology"]
    demand = measured["service_demand_at_full_500_qps"]

    enabled = [operation for operation in model["operations"] if operation["enabled"]]
    operation_qps = {operation["name"]: operation["reference_qps"] for operation in enabled}
    executable_qps = sum(operation_qps.values())
    total_business_qps = derived["total"]
    social_frontend_names = {"friend-list", "gen-share", "search-user", "list-friend-requests"}
    social_frontend_qps = sum(operation_qps.get(name, 0) for name in social_frontend_names)
    cross_auth_names = {"enter-friend", "water-cross", "weed-cross", "pest-cross", "steal"}
    cross_auth_qps = sum(operation_qps.get(name, 0) for name in cross_auth_names)
    social_calls_qps = (
        social_frontend_qps
        + derived["destructive_social_separate"]
        + cross_auth_qps
    )

    compute_utilization = policy["compute_utilization"]
    gateway_test = measured["gateway_connection_test"]
    gateway_connection_capacity = (
        gateway_test["connections_to_one_replica"]
        * policy["gateway_connection_utilization"]
    )
    gateway_by_connections = math.ceil(
        population["peak_concurrent_users"] / gateway_connection_capacity
    )
    gateway_by_cpu = math.ceil(
        total_business_qps
        * demand["gateway_cpu_ms_per_business_request"]
        / (1000 * topology["gateway"]["cpu_per_replica"] * compute_utilization)
    )
    gateway_base = max(gateway_by_connections, gateway_by_cpu)
    gateway_recommended = round_up(gateway_base + policy["failure_domain_spares"], 3)

    actor_ttl = measured["environment"]["farm_actor_ttl_seconds"]
    actor_resident = (
        population["peak_concurrent_users"]
        + population["peak_session_arrivals_per_second"] * actor_ttl
        + operation_qps["enter-friend"] * actor_ttl * policy["offline_friend_actor_share"]
    )
    actor_capacity = (
        measured["environment"]["farm_actor_max_resident"]
        * policy["farm_actor_utilization"]
    )
    farm_by_actors = math.ceil(actor_resident / actor_capacity)
    safe_mixed_qps = max(
        row["target_qps"] for row in measured["mixed_full"] if row["verdict"] == "pass"
    )
    farm_by_mixed_qps = math.ceil(executable_qps / (safe_mixed_qps * compute_utilization))
    farm_by_cpu = math.ceil(
        total_business_qps
        * demand["farm_cpu_ms_per_business_request"]
        / (1000 * topology["farm"]["cpu_per_replica"] * compute_utilization)
    )
    farm_base = max(farm_by_actors, farm_by_mixed_qps, farm_by_cpu)
    farm_recommended = round_up(farm_base + policy["failure_domain_spares"], 3)

    social_base = math.ceil(
        social_calls_qps
        / (measured["social_read_test"]["capacity_qps_pass"] * compute_utilization)
    )
    social_recommended = max(3, social_base + 1)

    mysql_qps = (
        total_business_qps * demand["mysql_statements_per_business_request"]
        + population["peak_session_arrivals_per_second"]
        * policy["mysql_auth_statements_per_session_start"]
    )
    mysql_capacity = measured["mysql_index_read_test"]["capacity_qps_pass"] * compute_utilization
    mysql_read_equivalent_primaries = math.ceil(mysql_qps / mysql_capacity)
    mysql_recommended_primaries = math.ceil(
        mysql_qps * policy["mysql_write_complexity_factor"] / mysql_capacity
    )

    redis = measured["redis_test"]
    redis_mixed_capacity = 1 / (
        redis["read_fraction"] / redis["get_qps"]
        + redis["write_fraction"] / redis["set_qps"]
    )
    redis_qps = (
        total_business_qps * demand["redis_commands_per_business_request"]
        + startup["redis_session_and_lease_commands_per_second"]
    )
    redis_primaries = math.ceil(redis_qps / (redis_mixed_capacity * compute_utilization))

    result = {
        "model": model["name"],
        "reference_load": {
            "dau": population["dau"],
            "pcu": population["peak_concurrent_users"],
            "business_qps": round(total_business_qps, 2),
            "farm_frontend_qps": round(executable_qps - social_frontend_qps, 2),
            "social_calls_qps_including_cross_auth": round(social_calls_qps, 2),
            "mysql_statement_qps": round(mysql_qps, 2),
            "redis_command_qps_including_sessions": round(redis_qps, 2),
            "estimated_resident_farm_actors": round(actor_resident),
        },
        "replicas": {
            "gateway": {
                "by_connections": gateway_by_connections,
                "by_cpu": gateway_by_cpu,
                "base": gateway_base,
                "recommended": gateway_recommended,
            },
            "farm": {
                "by_resident_actors": farm_by_actors,
                "by_current_full_mix_slo": farm_by_mixed_qps,
                "by_cpu": farm_by_cpu,
                "base": farm_base,
                "recommended": farm_recommended,
            },
            "social": {"base": social_base, "recommended": social_recommended},
            "mysql": {
                "read_equivalent_primary_shards": mysql_read_equivalent_primaries,
                "recommended_primary_shards": mysql_recommended_primaries,
                "recommended_replica_shards": mysql_recommended_primaries,
            },
            "redis": {
                "mixed_benchmark_qps": round(redis_mixed_capacity, 2),
                "recommended_primary_shards": redis_primaries,
                "recommended_replica_shards": redis_primaries,
            },
        },
        "gateway_to_farm_ratio": round(gateway_recommended / farm_recommended, 4),
    }
    with open(args.output, "w", encoding="utf-8") as output:
        json.dump(result, output, ensure_ascii=False, indent=2)
        output.write("\n")


if __name__ == "__main__":
    main()
