#!/usr/bin/env python3
"""采集容量实验A/B/C及恢复窗口，并计算可直接进入容量公式的资源增量。

A：无压测连接的固定基线；B：连接和Actor已经驻留但尚未发业务流量；
C：正式业务流量；D：停压后的后台排空窗口。脚本刻意同时保留服务聚合值和
逐Pod值，避免把副本平均百分比误当成整项服务资源消耗。
"""

from __future__ import annotations

import argparse
import json
import math
import statistics
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


SERVICES = {
    "gateway": {"container": "gateway", "pod": "gateway-.*", "go_service": "gateway"},
    "farm": {"container": "farm", "pod": "farm-.*", "go_service": "farm"},
    "social": {"container": "social", "pod": "social-.*", "go_service": "social"},
    "mysql": {"container": "mysql", "pod": "mysql-.*"},
    "redis": {"container": "redis", "pod": "redis-.*"},
    "benchstub": {"container": "benchstub", "pod": "benchstub-.*"},
    "load_generator": {"container": "k6", "pod": "k6-.*"},
}

GAUGES = {
    "gateway_ws_connections": 'sum(farm_ws_connections{service="gateway"})',
    "farm_actor_resident": 'sum(farm_actor_resident{service="farm"})',
    "farm_actor_mailbox_depth_p99": 'histogram_quantile(0.99, sum by (le) (rate(farm_actor_mailbox_depth_bucket{service="farm"}[30s])))',
    "farm_write_pending": 'sum(farm_write_journal_pending{service="farm"})',
    "farm_write_lag": 'sum(farm_write_journal_lag{service="farm"})',
    "farm_projection_active": 'sum(farm_write_journal_projection_active{service="farm"})',
    "farm_barrier_waiters": 'sum(farm_write_journal_barrier_waiters{service="farm"})',
    "farm_stream_queue_depth": 'sum(farm_grpc_stream_queue_depth{service="farm"})',
    "farm_stream_in_flight": 'sum(farm_grpc_stream_in_flight{service="farm"})',
    "farm_write_admission_limit": 'sum(farm_write_admission_limit{service="farm"})',
    "farm_projection_duration_p99_ms": '1000 * histogram_quantile(0.99, sum by (le) (rate(farm_write_journal_projection_duration_seconds_bucket{service="farm"}[30s])))',
    "farm_targeted_projection_duration_p99_ms": '1000 * histogram_quantile(0.99, sum by (le) (rate(farm_write_journal_targeted_projection_duration_seconds_bucket{service="farm"}[30s])))',
    "mysql_threads_connected": "sum(mysql_global_status_threads_connected)",
    "mysql_threads_running": "sum(mysql_global_status_threads_running)",
    "redis_connected_clients": "sum(redis_connected_clients)",
    "redis_used_memory_mib": "sum(redis_memory_used_bytes) / 1048576",
    "redis_dataset_memory_mib": "sum(redis_memory_used_dataset_bytes) / 1048576",
    "redis_rss_memory_mib": "sum(redis_memory_used_rss_bytes) / 1048576",
}

COUNTERS = {
    "gateway_rate_limited": 'farm_ws_rate_limited_total{service="gateway"}',
    "farm_stream_rejected": 'farm_grpc_stream_rejected_total{service="farm"}',
    "farm_write_admission_rejected": 'farm_write_admission_rejected_total{service="farm"}',
    "farm_committer_batches": 'farm_committer_batches_total{service="farm"}',
    "farm_committer_requests": 'farm_committer_requests_total{service="farm"}',
    "farm_journal_appends": 'farm_write_journal_appends_total{service="farm"}',
    "farm_journal_append_records": 'farm_write_journal_append_records_total{service="farm"}',
    "farm_projection_batches": 'farm_write_journal_projection_batches_total{service="farm"}',
    "farm_projection_records": 'farm_write_journal_projection_records_total{service="farm"}',
    "farm_journal_append_errors": 'farm_write_journal_append_errors_total{service="farm"}',
    "farm_projection_errors": 'farm_write_journal_projection_errors_total{service="farm"}',
    "farm_barrier_timeouts": 'farm_write_journal_barrier_timeouts_total{service="farm"}',
    "mysql_questions": "mysql_global_status_questions",
    "mysql_row_lock_waits": "mysql_global_status_innodb_row_lock_waits",
    "redis_commands": "redis_commands_processed_total",
    "redis_failed_commands": "redis_commands_failed_calls_total",
    "redis_rejected_commands": "redis_commands_rejected_calls_total",
    "redis_rejected_connections": "redis_rejected_connections_total",
}


def load(path: str) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def api(base_url: str, endpoint: str, parameters: dict[str, Any]) -> dict[str, Any]:
    query = urllib.parse.urlencode(parameters)
    url = f"{base_url.rstrip('/')}/api/v1/{endpoint}?{query}"
    with urllib.request.urlopen(url, timeout=30) as response:
        body = json.load(response)
    if body.get("status") != "success":
        raise RuntimeError(f"Prometheus query failed: {body}")
    return body.get("data", {})


def instant(base_url: str, expression: str, timestamp: float) -> list[dict[str, Any]]:
    data = api(base_url, "query", {"query": expression, "time": timestamp})
    return list(data.get("result", []))


def query_range(
    base_url: str,
    expression: str,
    start: float,
    end: float,
    step: int,
) -> list[dict[str, Any]]:
    data = api(
        base_url,
        "query_range",
        {"query": expression, "start": start, "end": end, "step": step},
    )
    return list(data.get("result", []))


def finite(value: Any) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return number if math.isfinite(number) else None


def values_from_series(series: list[dict[str, Any]]) -> list[float]:
    values: list[float] = []
    for item in series:
        for sample in item.get("values", []):
            value = finite(sample[1])
            if value is not None:
                values.append(value)
    return values


def summary(values: list[float]) -> dict[str, float | int | None]:
	ordered = sorted(values)
	p95_index = max(math.ceil(len(ordered) * 0.95) - 1, 0) if ordered else 0
	return {
		"samples": len(values),
		"average": statistics.fmean(values) if values else None,
		"p95": ordered[p95_index] if ordered else None,
		"peak": max(values) if values else None,
		"last": values[-1] if values else None,
	}


def range_summary(
    base_url: str,
    expression: str,
    start: float,
    end: float,
    step: int,
) -> dict[str, Any]:
    return summary(values_from_series(query_range(base_url, expression, start, end, step)))


def per_pod_range_summary(
    base_url: str,
    expression: str,
    start: float,
    end: float,
    step: int,
) -> dict[str, dict[str, float | int | None]]:
    output: dict[str, dict[str, float | int | None]] = {}
    for item in query_range(base_url, expression, start, end, step):
        pod = str(item.get("metric", {}).get("pod", "unknown"))
        output[pod] = summary(values_from_series([item]))
    return output


def instant_sum(
    base_url: str,
    expression: str,
    timestamp: float,
    empty_value: float | None = None,
) -> float:
    total = 0.0
    found = False
    for item in instant(base_url, expression, timestamp):
        value = finite(item["value"][1])
        if value is not None:
            total += value
            found = True
    if not found and empty_value is not None:
        return empty_value
    if not found:
        raise RuntimeError(f"Prometheus query returned no finite samples: {expression}")
    return total


def instant_by_pod(base_url: str, expression: str, timestamp: float) -> dict[str, float]:
    output: dict[str, float] = {}
    for item in instant(base_url, expression, timestamp):
        value = finite(item["value"][1])
        if value is None:
            continue
        pod = str(item.get("metric", {}).get("pod", "unknown"))
        output[pod] = output.get(pod, 0.0) + value
    if not output:
        raise RuntimeError(f"Prometheus query returned no finite pod samples: {expression}")
    return output


def prom_duration(seconds: float) -> str:
    # PromQL接受毫秒，但实验窗口均按整秒设计；向下只会丢失不到一个采样周期的
    # 边界小数，且可以避免浮点字符串被Prometheus版本差异拒绝。
    return f"{max(int(round(seconds)), 1)}s"


def counter_window(
    base_url: str,
    selector: str,
    start: float,
    end: float,
    group_by_pod: bool = False,
    missing_is_zero: bool = False,
) -> dict[str, Any]:
    duration = end - start
    range_selector = prom_duration(duration)
    aggregate_expression = f"sum(increase({selector}[{range_selector}]))"
    total = instant_sum(
        base_url,
        aggregate_expression,
        end,
        empty_value=0.0 if missing_is_zero else None,
    )
    output: dict[str, Any] = {
        "increase": total,
        "per_second": total / duration,
    }
    if group_by_pod:
        pod_expression = f"sum by (pod) (increase({selector}[{range_selector}]))"
        per_pod = instant_by_pod(base_url, pod_expression, end)
        output["per_pod_increase"] = per_pod
        output["per_pod_per_second"] = {
            pod: value / duration for pod, value in per_pod.items()
        }
    return output


def safe(output: dict[str, Any], errors: dict[str, str], name: str, operation: Any) -> None:
    try:
        output[name] = operation()
    except Exception as error:  # 留下部分证据，不因一个exporter缺指标丢掉整轮实验。
        output[name] = None
        errors[name] = str(error)


def service_window(
    base_url: str,
    service: str,
    config: dict[str, str],
    start: float,
    end: float,
    step: int,
) -> dict[str, Any]:
    container = config["container"]
    pod = config["pod"]
    selector = f'{{namespace="benkz",container="{container}",pod=~"{pod}"}}'
    duration = end - start
    output: dict[str, Any] = {}
    errors: dict[str, str] = {}

    go_service = config.get("go_service")
    if go_service:
        # 应用自身的进程计数器与业务metrics同一次抓取。在Farm高负载时，
        # cAdvisor偶尔会在恰好30秒的窗口中只留下一个样本，increase会返回空；
        # 进程计数器仍有完整样本，且正好代表该业务进程实际消耗的CPU。
        cpu_selector = f'process_cpu_seconds_total{{service="{go_service}"}}'
        cpu_source = "process_cpu_seconds_total"
    else:
        cpu_selector = f"container_cpu_usage_seconds_total{selector}"
        cpu_source = "container_cpu_usage_seconds_total"

    def cpu() -> dict[str, Any]:
        result: dict[str, Any] = {
            "source": cpu_source,
            "smoothed_30s": range_summary(
                base_url,
                f"sum(rate({cpu_selector}[30s]))",
                start,
                end,
                step,
            ),
            "per_pod_smoothed_30s": per_pod_range_summary(
                base_url,
                f"sum by (pod) (rate({cpu_selector}[30s]))",
                start,
                end,
                step,
            ),
        }
        try:
            counters = counter_window(
                base_url, cpu_selector, start, end, group_by_pod=True
            )
            result.update(counters)
            result["average_cores"] = counters["increase"] / duration
        except Exception as error:
            # 30秒窗口偶尔只覆盖一个cAdvisor原始样本。保留平滑曲线用于
            # 饱和诊断，并由跨度至少60秒的CD完整核算窗口给出单位成本。
            result.update({"increase": None, "per_second": None, "average_cores": None})
            errors["cpu_counter"] = str(error)
        return result

    safe(output, errors, "cpu", cpu)

    if go_service:
        container_cpu_selector = f"container_cpu_usage_seconds_total{selector}"
        safe(
            output,
            errors,
            "container_cpu_cross_check",
            lambda: counter_window(
                base_url,
                container_cpu_selector,
                start,
                end,
                group_by_pod=True,
            ),
        )

    memory_selector = f"container_memory_working_set_bytes{selector}"
    # cAdvisor会在容器退出后短暂保留最后一条working-set样本。滚动重启后若
    # 直接sum，旧ReplicaSet与新Pod会被同时计入，A/B/C内存甚至可能出现
    # “负增量”。container_last_seen由同一次cAdvisor抓取产生；在每个
    # query_range采样点只保留20秒内仍被观察到的容器。必须把容器id也放入
    # 匹配键：StatefulSet滚动重启前后Pod名不变，如果只按Pod和容器名匹配，
    # 新容器的container_last_seen会错误地让旧容器的内存序列继续存活。
    # 抓取周期为15秒，
    # 该窗口允许少量抓取抖动，同时会在下一次采样前剔除已退出Pod；更宽的
    # 45秒窗口仍会把滚动更新旧Pod带入A窗口的第一个样本。
    live_memory_selector = (
        f"({memory_selector} and on(namespace,pod,container,id) "
        f"((time() - container_last_seen{selector}) < 20))"
    )
    safe(
        output,
        errors,
        "memory_working_set_mib",
        lambda: {
            "aggregate": range_summary(
                base_url,
                f"sum({live_memory_selector}) / 1048576",
                start,
                end,
                step,
            ),
            "per_pod": per_pod_range_summary(
                base_url,
                f"sum by (pod) ({live_memory_selector}) / 1048576",
                start,
                end,
                step,
            ),
        },
    )

    throttled_seconds = f"container_cpu_cfs_throttled_seconds_total{selector}"
    throttled_periods = f"container_cpu_cfs_throttled_periods_total{selector}"
    periods = f"container_cpu_cfs_periods_total{selector}"

    def throttling() -> dict[str, Any]:
        seconds = counter_window(base_url, throttled_seconds, start, end, group_by_pod=True)
        throttled = counter_window(base_url, throttled_periods, start, end, group_by_pod=True)
        all_periods = counter_window(base_url, periods, start, end, group_by_pod=True)
        total_periods = all_periods["increase"]
        return {
            "throttled_seconds": seconds["increase"],
            "throttled_core_ratio": seconds["increase"] / duration,
            "throttled_periods": throttled["increase"],
            "periods": total_periods,
            "throttled_period_ratio": throttled["increase"] / total_periods if total_periods else 0.0,
            "per_pod_throttled_seconds": seconds["per_pod_increase"],
        }

    safe(output, errors, "throttling", throttling)

    for direction in ("receive", "transmit"):
        network_selector = (
            f'container_network_{direction}_bytes_total'
            f'{{namespace="benkz",pod=~"{pod}"}}'
        )
        safe(
            output,
            errors,
            f"network_{direction}_bytes",
            lambda selector=network_selector: counter_window(
                base_url, selector, start, end, group_by_pod=True
            ),
        )

    if go_service:
        safe(
            output,
            errors,
            "go_allocated_bytes",
            lambda: counter_window(
                base_url,
                f'go_memstats_alloc_bytes_total{{service="{go_service}"}}',
                start,
                end,
                group_by_pod=True,
            ),
        )
    output["errors"] = errors
    return output


def metric_window(
    base_url: str,
    start: float,
    end: float,
    step: int,
) -> dict[str, Any]:
    output: dict[str, Any] = {"gauges": {}, "counters": {}, "errors": {}}
    for name, expression in GAUGES.items():
        safe(
            output["gauges"],
            output["errors"],
            name,
            lambda expression=expression: range_summary(
                base_url, expression, start, end, step
            ),
        )
    for name, selector in COUNTERS.items():
        safe(
            output["counters"],
            output["errors"],
            name,
            lambda selector=selector: counter_window(
                base_url, selector, start, end, missing_is_zero=True
            ),
        )
    return output


def node_window(base_url: str, start: float, end: float, step: int) -> dict[str, Any]:
    output: dict[str, Any] = {}
    errors: dict[str, str] = {}
    duration = end - start

    def cpu() -> dict[str, Any]:
        used = counter_window(
            base_url,
            'container_cpu_usage_seconds_total{id="/"}',
            start,
            end,
        )
        cores = instant_sum(base_url, "machine_cpu_cores", end)
        average = used["increase"] / duration
        return {
            **used,
            "machine_cores": cores,
            "average_used_cores": average,
            "average_utilization_ratio": average / cores,
            "smoothed_30s": range_summary(
                base_url,
                'sum(rate(container_cpu_usage_seconds_total{id="/"}[30s]))',
                start,
                end,
                step,
            ),
        }

    safe(output, errors, "cpu", cpu)
    safe(
        output,
        errors,
        "memory",
        lambda: {
            "machine_mib": instant_sum(base_url, "machine_memory_bytes", end) / 1048576,
            "working_set_mib": range_summary(
                base_url,
                'sum(container_memory_working_set_bytes{id="/"}) / 1048576',
                start,
                end,
                step,
            ),
        },
    )
    output["errors"] = errors
    return output


def collect_window(
    base_url: str,
    name: str,
    start: float,
    end: float,
    step: int,
) -> dict[str, Any]:
    if end <= start:
        raise ValueError(f"window {name} has invalid bounds: {start}..{end}")
    return {
        "name": name,
        "start_unix_seconds": start,
        "end_unix_seconds": end,
        "duration_seconds": end - start,
        "services": {
            service: service_window(base_url, service, config, start, end, step)
            for service, config in SERVICES.items()
        },
        "system_metrics": metric_window(base_url, start, end, step),
        "node": node_window(base_url, start, end, step),
    }


def nested_average(window: dict[str, Any], service: str, metric: str) -> float | None:
    try:
        if metric == "cpu":
            cpu = window["services"][service]["cpu"]
            value = finite(cpu.get("average_cores"))
            if value is not None:
                return value
            # cAdvisor 的原始计数器在一个恰好跨两次抓取的短窗口内偶尔会让
            # increase() 返回空，但同一窗口的 30 秒 rate 曲线仍有多个有限
            # 样本。它比用 0 或丢弃整轮实验更符合实际，也只在计数器缺样时
            # 回退；正常轮次仍以精确 increase / duration 为准。
            return finite(cpu.get("smoothed_30s", {}).get("average"))
        value = window["services"][service]["memory_working_set_mib"]["aggregate"]["average"]
        return float(value) if value is not None else None
    except (KeyError, TypeError, ValueError):
        return None


def nested_memory_sample(
    window: dict[str, Any], service: str, sample: str
) -> float | None:
    try:
        value = window["services"][service]["memory_working_set_mib"]["aggregate"][sample]
        return float(value) if value is not None else None
    except (KeyError, TypeError, ValueError):
        return None


def derive(windows: dict[str, dict[str, Any]], result: dict[str, Any]) -> dict[str, Any]:
    actual_qps = float(result.get("succeeded", 0)) / (
        float(result["measurement_millis"]) / 1000
    )
    output: dict[str, Any] = {
        "successful_qps": actual_qps,
        "services": {},
    }
    for service in ("gateway", "farm", "social", "mysql", "redis"):
        cpu_a = nested_average(windows["A_idle"], service, "cpu")
        cpu_b = nested_average(windows["B_state"], service, "cpu")
        cpu_c = nested_average(windows["C_load"], service, "cpu")
        mem_a_average = nested_average(windows["A_idle"], service, "memory")
        mem_b_average = nested_average(windows["B_state"], service, "memory")
        mem_c_average = nested_average(windows["C_load"], service, "memory")
        mem_a_last = nested_memory_sample(windows["A_idle"], service, "last")
        mem_b_last = nested_memory_sample(windows["B_state"], service, "last")
        mem_c_peak = nested_memory_sample(windows["C_load"], service, "peak")
        business_cpu = (
            max(cpu_c - cpu_b, 0.0)
            if cpu_b is not None and cpu_c is not None
            else None
        )
        c_cpu = windows["C_load"]["services"][service].get("cpu")
        drain_cpu = windows["D_drain_accounting"]["services"][service].get("cpu")
        complete_cpu = windows["CD_complete_accounting"]["services"][service].get("cpu")
        c_seconds = (
            float(c_cpu["increase"])
            if c_cpu and c_cpu.get("increase") is not None
            else None
        )
        drain_seconds = (
            float(drain_cpu["increase"])
            if drain_cpu and drain_cpu.get("increase") is not None
            else None
        )
        combined_seconds = (
            float(complete_cpu["increase"])
            if complete_cpu and complete_cpu.get("increase") is not None
            else None
        )
        c_duration = float(windows["C_load"]["duration_seconds"])
        drain_duration = float(windows["D_drain_accounting"]["duration_seconds"])
        # WebSocket已经在D开始时关闭，所以Gateway/Social按空闲A扣基线；
        # Farm Actor和数据库连接池仍驻留，后端服务按状态窗口B扣基线。
        drain_baseline = cpu_a if service in ("gateway", "social") else cpu_b
        synchronous_cpu_seconds = (
            max(c_seconds - cpu_b * c_duration, 0.0)
            if c_seconds is not None and cpu_b is not None
            else None
        )
        deferred_cpu_seconds = (
            max(drain_seconds - drain_baseline * drain_duration, 0.0)
            if drain_seconds is not None and drain_baseline is not None
            else None
        )
        combined_baseline_seconds = (
            cpu_b * c_duration + drain_baseline * drain_duration
            if cpu_b is not None and drain_baseline is not None
            else None
        )
        combined_complete_cpu_seconds = (
            max(combined_seconds - combined_baseline_seconds, 0.0)
            if combined_seconds is not None and combined_baseline_seconds is not None
            else None
        )
        split_complete_cpu_seconds = (
            synchronous_cpu_seconds + deferred_cpu_seconds
            if synchronous_cpu_seconds is not None and deferred_cpu_seconds is not None
            else None
        )
        complete_candidates = [
            value
            for value in (combined_complete_cpu_seconds, split_complete_cpu_seconds)
            if value is not None
        ]
        # Prometheus在刚好跨过一次抓取缺口时，长CD increase可能反而小于
        # 已经成功算出的C increase。完整成本不可能小于同步成本，因此取
        # “长窗口核算”和“C+D分段核算”的较大值，避免低估后台投影CPU。
        complete_cpu_seconds = max(complete_candidates) if complete_candidates else None
        if complete_cpu_seconds is not None and synchronous_cpu_seconds is not None:
            deferred_cpu_seconds = max(complete_cpu_seconds - synchronous_cpu_seconds, 0.0)
        output["services"][service] = {
            "fixed_cpu_cores_A": cpu_a,
            "state_cpu_cores_B_minus_A": (
                cpu_b - cpu_a if cpu_a is not None and cpu_b is not None else None
            ),
            "business_cpu_cores_C_minus_B": business_cpu,
            "business_cpu_milliseconds_per_successful_request": (
                business_cpu * 1000 / actual_qps
                if business_cpu is not None and actual_qps > 0
                else None
            ),
            "synchronous_business_cpu_seconds": synchronous_cpu_seconds,
            "deferred_drain_cpu_seconds": deferred_cpu_seconds,
            "complete_business_cpu_seconds": complete_cpu_seconds,
            "complete_cpu_accounting_window_seconds": (
                float(windows["CD_complete_accounting"]["duration_seconds"])
            ),
            "complete_cpu_milliseconds_per_successful_request": (
                complete_cpu_seconds * 1000 / float(result.get("succeeded", 0))
                if complete_cpu_seconds is not None and int(result.get("succeeded", 0)) > 0
                else None
            ),
            # CPU是区间平均资源；内存是容量占用，不能用连接刚建立时偏低的
            # B窗口平均值。固定内存取A末值，状态内存取B末值差，业务工作集
            # 取C峰值相对B末值的增量，并同时保留平均值供检查。
            "fixed_memory_mib_A": mem_a_last,
            "state_memory_mib_B_minus_A": (
                max(mem_b_last - mem_a_last, 0.0)
                if mem_a_last is not None and mem_b_last is not None
                else None
            ),
            "load_memory_mib_C_minus_B": (
                max(mem_c_peak - mem_b_last, 0.0)
                if mem_b_last is not None and mem_c_peak is not None
                else None
            ),
            "memory_diagnostics_mib": {
                "A_average": mem_a_average,
                "A_last": mem_a_last,
                "B_average": mem_b_average,
                "B_last": mem_b_last,
                "C_average": mem_c_average,
                "C_peak": mem_c_peak,
            },
        }
    return output


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:30909")
    parser.add_argument("--context", required=True, help="包含idle/recovery时间戳的JSON")
    parser.add_argument("--result", required=True, help="servicebench结果JSON")
    parser.add_argument("--output", required=True)
    parser.add_argument("--step", type=int, default=5)
    args = parser.parse_args()

    context = load(args.context)
    result = load(args.result)
    measurement_start = float(result["measurement_start_unix_ms"]) / 1000
    measurement_duration = float(result["measurement_millis"]) / 1000
    state_start = float(result["state_ready_unix_ms"]) / 1000
    measurement_end = measurement_start + measurement_duration
    recovery_end = float(context["recovery_end_unix_ms"]) / 1000
    journal_idle = float(context["journal_idle_unix_ms"]) / 1000
    # 至少保留30秒，确保15秒抓取周期下有足够计数器样本；超过实际排空点的
    # 空闲尾巴会在derive中按基线CPU扣除。
    drain_accounting_end = min(recovery_end, max(journal_idle, measurement_end + 30))
    windows_spec = {
        "A_idle": (
            float(context["idle_start_unix_ms"]) / 1000,
            float(context["idle_end_unix_ms"]) / 1000,
        ),
        "B_state": (state_start, measurement_start),
        "C_load": (measurement_start, measurement_end),
        "D_drain_accounting": (measurement_end, drain_accounting_end),
        "CD_complete_accounting": (measurement_start, drain_accounting_end),
        "D_recovery": (measurement_end, recovery_end),
    }
    windows = {
        name: collect_window(args.url, name, start, end, args.step)
        for name, (start, end) in windows_spec.items()
    }
    output = {
        "method": "A idle / B state / C business / D recovery exact benchmark windows",
        "prometheus_url": args.url,
        "source_context": str(Path(args.context).resolve()),
        "source_result": str(Path(args.result).resolve()),
        "result_summary": {
            "target_qps": int(result["target_qps"]),
            "sent": int(result["sent"]),
            "succeeded": int(result["succeeded"]),
            "failed": int(result["failed"]),
            "client_drain_milliseconds": int(result.get("drain_millis", 0)),
        },
        "windows": windows,
        "derived": derive(windows, result),
        "notes": [
            "CPU average uses Prometheus counter increase divided by exact window duration.",
            "CPU peak is a 30-second smoothed diagnostic and is not used as the unit-cost slope.",
            "C-minus-B isolates business CPU; B-minus-A isolates connection/Actor state cost.",
            "Complete per-request CPU adds post-C drain work and subtracts the matching state baseline.",
            "Memory coefficients use A-last, B-last and C-peak rather than window averages.",
            "All service resource values are aggregate sums; per-Pod values remain for skew checks.",
            "Container memory excludes cAdvisor samples whose container_last_seen is older than 20 seconds.",
        ],
    }
    Path(args.output).write_text(
        json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
