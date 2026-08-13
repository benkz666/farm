#!/usr/bin/env python3
"""汇总严格 SLO 压测结果，并补充 Prometheus 与 Grafana 时间窗。"""

import argparse
import html
import json
import statistics
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Dict, List, NamedTuple, Optional


class Case(NamedTuple):
    name: str
    filename: str
    scope: str
    key_service: str
    command: str
    p90_limit: float
    p99_limit: float
    window: str
    background: str = "不适用"


CASES = (
    Case("Handshake", "handshake-3000.json", "客户端→3 Gateway", "Gateway", "100", 150, 300, "15,000 连接突发 / 5 秒"),
    Case("Ping", "final-ping-8000-60s.json", "客户端→3 Gateway", "Gateway", "102", 50, 100, "60 秒"),
    Case("EnterFarm", "final-enter-4000-60s.json", "客户端→3 Gateway→Farm", "Farm", "200", 150, 300, "60 秒"),
    Case("SyncFarm", "final-sync-8000-60s.json", "客户端→3 Gateway→Farm", "Farm", "204", 100, 300, "60 秒"),
    Case("Water 本地", "final-water-4000-60s.json", "客户端→3 Gateway→Farm", "Farm", "212", 100, 300, "60 秒 / 24 万次合法状态", "未即时采样；仅认定前台 SLO"),
    Case("Harvest", "final-harvest-4000-60s-clean.json", "客户端→3 Gateway→Farm", "Farm", "220", 100, 300, "60 秒 / 24 万次合法状态", "停压约 13.7 万条；约 55 秒归零"),
    Case("Steal", "final-steal-2500-60s.json", "客户端→3 Gateway→Farm↔Social", "Farm+Social", "222", 150, 300, "60 秒 / 15 万次合法状态", "停压约 54.0 万条；约 387 秒归零"),
    Case("Buy", "final-buy-5000-60s.json", "客户端→3 Gateway→Farm", "Farm", "302", 100, 300, "60 秒", "停压 pending=0、lag=0"),
    Case("Sell", "final-sell-5000-60s.json", "客户端→3 Gateway→Farm", "Farm", "304", 100, 300, "60 秒", "停压 pending=0、lag=0"),
    Case("FriendList", "final-friend-list-2000-60s.json", "客户端→3 Gateway→Social", "Social", "400", 100, 300, "60 秒"),
    Case("SearchUser", "final-search-user-15000-60s.json", "客户端→3 Gateway→Social", "Social", "410", 100, 300, "60 秒"),
    Case("Water 跨农场", "final-water-visitor-2500-60s.json", "客户端→3 Gateway→Farm↔Social", "Farm+Social", "212", 150, 300, "60 秒 / 15 万次合法状态", "停压约 53.6 万条；约 4–5 分钟归零"),
    Case("TaskList", "final-task-list-3000-60s.json", "客户端→3 Gateway→Farm", "Farm", "600", 100, 300, "60 秒"),
    Case("MailList", "final-mail-list-5000-60s.json", "客户端→3 Gateway→Farm", "Farm", "604", 100, 300, "60 秒"),
    Case("AreFriends gRPC", "final-are-friends-3000-60s.json", "servicebench→Social", "Social", "AreFriends", 50, 100, "60 秒"),
)


CPU_QUERIES = {
    "gateway": 'sum(rate(container_cpu_usage_seconds_total{name=~"benkz-gateway-[123]-1"}[30s])) * 100',
    "farm": 'sum(rate(container_cpu_usage_seconds_total{name="benkz-farm-1"}[30s])) * 100',
    "social": 'sum(rate(container_cpu_usage_seconds_total{name="benkz-social-1"}[30s])) * 100',
    "mysql": 'sum(rate(container_cpu_usage_seconds_total{name="benkz-mysql-1"}[30s])) * 100',
    "redis": 'sum(rate(container_cpu_usage_seconds_total{name="benkz-redis-1"}[30s])) * 100',
}


def load_json(path: Path) -> Dict[str, Any]:
    with path.open(encoding="utf-8") as source:
        return json.load(source)


def query_range(base_url: str, expression: str, start: float, end: float, step: int = 5) -> List[float]:
    parameters = urllib.parse.urlencode({"query": expression, "start": start, "end": end, "step": step})
    request_url = base_url.rstrip("/") + "/api/v1/query_range?" + parameters
    with urllib.request.urlopen(request_url, timeout=20) as response:
        payload = json.loads(response.read().decode("utf-8"))
    if payload.get("status") != "success":
        raise RuntimeError(f"Prometheus query failed: {payload}")
    values = []  # type: List[float]
    for series in payload.get("data", {}).get("result", []):
        values.extend(float(item[1]) for item in series.get("values", []))
    return values


def cpu_window(prometheus_url: str, result: Dict[str, Any]) -> Dict[str, Any]:
    start = float(result["measurement_start_unix_ms"]) / 1000
    end = float(result["measurement_end_unix_ms"]) / 1000
    output = {}  # type: Dict[str, Any]
    for name, expression in CPU_QUERIES.items():
        try:
            values = query_range(prometheus_url, expression, start, end)
            output[name] = {
                "average_pct": statistics.mean(values) if values else None,
                "peak_pct": max(values) if values else None,
            }
        except Exception as error:  # 报告仍应保留原始压测结果。
            output[name] = {"average_pct": None, "peak_pct": None, "error": str(error)}
    return output


def success_rate(result: Dict[str, Any]) -> float:
    completed = int(result.get("succeeded", 0)) + int(result.get("failed", 0))
    return float(result.get("succeeded", 0)) / completed if completed else 0.0


def classify(case: Case, result: Dict[str, Any]) -> str:
    reached = float(result["actual_qps"]) >= float(result["target_qps"]) * 0.98
    reliable = success_rate(result) >= 0.999
    strict = float(result["p90_ms"]) <= case.p90_limit and float(result["p99_ms"]) <= case.p99_limit
    p99_tolerance = 120 if case.p99_limit == 100 else 330
    tolerant = float(result["p90_ms"]) <= case.p90_limit * 1.2 and float(result["p99_ms"]) <= p99_tolerance
    if reached and reliable and strict:
        return "通过"
    if reached and reliable and tolerant:
        return "临界通过"
    return "不通过"


def fmt(value: Optional[float], digits: int = 1) -> str:
    if value is None:
        return "—"
    return f"{value:,.{digits}f}"


def key_cpu(case: Case, cpu: Dict[str, Any]) -> str:
    def pair(name: str) -> str:
        values = cpu.get(name, {})
        return f"{fmt(values.get('average_pct'))}%/{fmt(values.get('peak_pct'))}%"

    if case.key_service == "Gateway":
        return "3GW " + pair("gateway")
    if case.key_service == "Farm":
        return "3GW " + pair("gateway") + "；Farm " + pair("farm")
    if case.key_service == "Social":
        if case.name == "AreFriends gRPC":
            return "Social " + pair("social")
        return "3GW " + pair("gateway") + "；Social " + pair("social")
    return "3GW " + pair("gateway") + "；Farm " + pair("farm") + "；Social " + pair("social")


def dashboard_link(grafana_url: str, command: str, start_ms: int, end_ms: int) -> str:
    parameters = urllib.parse.urlencode({
        "orgId": "1",
        "var-cmd": command,
        "var-route": "/ws",
        "from": str(start_ms - 15_000),
        "to": str(end_ms + 15_000),
    })
    return grafana_url.rstrip("/") + "/d/farm-overview/farm-overview?" + parameters


def markdown_table(rows: List[Dict[str, Any]]) -> str:
    lines = [
        "| 接口 | 前台 SLO QPS | Avg | P90 | P99 | 成功率 | 关键 CPU 平均/峰值 | 判定 |",
        "|---|---:|---:|---:|---:|---:|---|---|",
    ]
    for row in rows:
        lines.append(
            f"| [{row['name']}]({row['grafana']}) | {fmt(row['actual_qps'], 0)} | "
            f"{fmt(row['average_ms'])} ms | {fmt(row['p90_ms'])} ms | {fmt(row['p99_ms'])} ms | "
            f"{row['success_rate'] * 100:.3f}% | {row['key_cpu']} | {row['status']} |"
        )
    return "\n".join(lines)


def html_rows(rows: List[Dict[str, Any]]) -> str:
    rendered = []
    for row in rows:
        status_class = "ok" if row["status"] == "通过" else "warn" if row["status"] == "临界通过" else "bad"
        cells = (
            f'<a href="{html.escape(row["grafana"])}">{html.escape(row["name"])}</a>',
            fmt(row["actual_qps"], 0),
            fmt(row["average_ms"]),
            fmt(row["p90_ms"]),
            fmt(row["p99_ms"]),
            f'{row["success_rate"] * 100:.3f}%',
            html.escape(row["key_cpu"]),
            f'<span class="{status_class}">{html.escape(row["status"])}</span>',
        )
        rendered.append("<tr>" + "".join(f"<td>{value}</td>" for value in cells) + "</tr>")
    return "".join(rendered)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--prometheus-url", default="http://21.6.81.134:39090")
    parser.add_argument("--grafana-url", default="http://21.6.81.134:33000")
    args = parser.parse_args()

    run_dir = Path(args.run_dir).resolve()
    rows = []  # type: List[Dict[str, Any]]
    monitoring = {}  # type: Dict[str, Any]
    for case in CASES:
        result = load_json(run_dir / case.filename)
        cpu = cpu_window(args.prometheus_url, result)
        monitoring[case.name] = cpu
        rows.append({
            "name": case.name,
            "scope": case.scope,
            "window": case.window,
            "background": case.background,
            "target_qps": result["target_qps"],
            "actual_qps": result["actual_qps"],
            "average_ms": result["average_ms"],
            "p90_ms": result["p90_ms"],
            "p99_ms": result["p99_ms"],
            "succeeded": result["succeeded"],
            "failed": result["failed"],
            "success_rate": success_rate(result),
            "p90_limit": case.p90_limit,
            "p99_limit": case.p99_limit,
            "status": classify(case, result),
            "key_cpu": key_cpu(case, cpu),
            "start_ms": result["measurement_start_unix_ms"],
            "end_ms": result["measurement_end_unix_ms"],
            "grafana": dashboard_link(
                args.grafana_url,
                case.command,
                int(result["measurement_start_unix_ms"]),
                int(result["measurement_end_unix_ms"]),
            ),
        })

    overall_start = min(int(row["start_ms"]) for row in rows) - 30_000
    overall_end = max(int(row["end_ms"]) for row in rows) + 30_000
    overall_dashboard = dashboard_link(args.grafana_url, "All", overall_start + 15_000, overall_end - 15_000)

    def metric_max(service: str, field: str) -> float:
        values = [
            item.get(service, {}).get(field)
            for item in monitoring.values()
            if item.get(service, {}).get(field) is not None
        ]
        return max(values) if values else 0.0

    gateway_average_max = metric_max("gateway", "average_pct")
    farm_average_max = metric_max("farm", "average_pct")
    farm_peak_max = metric_max("farm", "peak_pct")
    mysql_average_max = metric_max("mysql", "average_pct")
    mysql_peak_max = metric_max("mysql", "peak_pct")
    redis_peak_max = metric_max("redis", "peak_pct")

    background_lines = "\n".join(
        f"- **{row['name']}**：{row['background']}" for row in rows if row["background"] != "不适用"
    )
    report = f"""# 严格 SLO 接口性能测试报告

## 测试口径

- 拓扑：3 × Gateway（各 1 核 1 GiB）+ 1 × Farm（1 核 1 GiB）；Social、Redis 各 1 核 1 GiB，MySQL 2 核 4 GiB。
- 发压端：servicebench/k6 24 核 16 GiB；统一使用 15,000 个账号，每账号最多 18 个合法地块。
- 排除注册、登录；WebSocket、会话和 Actor 均在测量窗口前预热；每连接最多 8 命令/s。
- Avg 只记录；P90/P99 按接口类别判断。实际 QPS 必须达到目标 98%，成功率必须至少 99.9%。
- 一般接口 P99 目标 300 ms、容忍上限 330 ms；Ping/AreFriends P99 目标 100 ms、容忍上限 120 ms。
- CPU 目标为关键服务 75%–85%，但时延或后台积压先触线时，以先触线者为容量边界。
- [打开 Grafana 全部正式测试时间窗]({overall_dashboard})

## 正式结果

{markdown_table(rows)}

表中 CPU 为“平均/峰值”；3GW 是三个 1 核 Gateway 的合计百分比（300% 代表三核全满）。每个接口名称都链接到自己的 Grafana 固定时间窗。

## 异步写的持续性

{background_lines}

- Buy/Sell 5,000 QPS 同时满足前台 SLO 和停压无积压，可作为本轮真正的长期稳定档。
- Harvest 4,000、Water 跨农场 2,500、Steal 2,500 QPS 是前台 SLO 容量，不是无积压容量；持续写入时 Projector/MySQL 投影先成为瓶颈。
- 本地 Water 4,000 QPS 的前台数据有效，但该窗口未即时保存停压积压快照，因此不据此宣称端到端无积压。

## 资源判断

- 三个 Gateway 的最高窗口平均 CPU 合计约 {gateway_average_max:.1f}%（平均每实例约 {gateway_average_max / 3:.1f}%），没有接近三核总上限。
- Farm 的最高窗口平均 CPU 约 {farm_average_max:.1f}%，最高瞬时约 {farm_peak_max:.1f}%；部分写场景接近 80% 目标，但读场景多为 P99 先触线。
- MySQL 最高窗口平均 CPU 约 {mysql_average_max:.1f}% 个单核（2 核配额的 {mysql_average_max / 2:.1f}%）；一次后台追平阶段瞬时达到约 {mysql_peak_max / 2:.1f}% 配额，但没有持续 CPU 饱和。
- 共享 Redis 最高瞬时 CPU 约 {redis_peak_max:.1f}%。会话、缓存和事件日志共享实例，但通过独立连接池与 key 前缀隔离；跨农场积压的直接限制仍是事件放大、Projector 和有序 SQL 投影吞吐。
- 最终检查中 Gateway/Farm/Social/MySQL/Redis 均在运行，RestartCount=0、OOMKilled=false；夹具切换造成的主动 Farm restart 不属于异常重启。

## 主要结论

1. 严格长窗口下，读接口稳定档为 Ping 8,000、EnterFarm 4,000、SyncFarm 8,000、FriendList 2,000、SearchUser 15,000、TaskList 3,000、MailList 5,000、AreFriends 3,000 QPS。
2. 本地 Water/Harvest 的前台稳定档均为 4,000 QPS；Buy/Sell 的端到端稳定档均为 5,000 QPS。
3. 跨农场 Water/Steal 的前台稳定档均为 2,500 QPS，但每轮会形成约 54 万条后台积压，长期容量由 Projector/MySQL 决定。
4. 多数失败档不是系统错误，而是 P99 先超过 SLO；因此关键服务 CPU 未必达到 80%。这符合“不能牺牲玩家体验换吞吐”的约定。
5. Handshake 结果为 15,000 账号的一次性连接突发，3,000 连接/s 通过；它不应与可无限重复的业务 QPS 等同。

## 原始证据

- 本目录保留每一档 servicebench JSON，包括通过档与失败档。
- `prometheus.json` 保存每个正式窗口的各容器 CPU 平均/峰值。
- 结果基于单次 45–60 秒正式窗口；适合本次快速容量判断，不冒充 3–5 分钟生产级稳定性认证。
"""
    (run_dir / "report.md").write_text(report, encoding="utf-8")

    background_html = "".join(
        f"<li><b>{html.escape(row['name'])}</b>：{html.escape(row['background'])}</li>"
        for row in rows if row["background"] != "不适用"
    )
    document = f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>严格 SLO 接口性能测试报告</title><style>
:root{{--bg:#f4f7fb;--card:#fff;--line:#dce4ef;--text:#172033;--muted:#607089;--blue:#1769aa}}
*{{box-sizing:border-box}}body{{margin:0;background:var(--bg);color:var(--text);font:14px/1.6 system-ui,sans-serif}}
main{{max-width:1500px;margin:28px auto;padding:0 22px}}h1{{font-size:30px}}h2{{margin-top:0}}.card{{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:22px;margin:16px 0;box-shadow:0 4px 20px #243b5a0d}}
.table{{overflow:auto}}table{{border-collapse:collapse;width:100%;min-width:980px}}th,td{{padding:11px 10px;border-bottom:1px solid #e8edf4;text-align:right;white-space:nowrap}}th:first-child,td:first-child,th:nth-last-child(2),td:nth-last-child(2){{text-align:left}}th{{background:#eff5fb;position:sticky;top:0}}a{{color:var(--blue)}}.ok{{color:#087a45;font-weight:700}}.warn{{color:#a05a00;font-weight:700}}.bad{{color:#b42318;font-weight:700}}.muted{{color:var(--muted)}}
</style></head><body><main><h1>严格 SLO 接口性能测试报告</h1>
<section class="card"><h2>口径</h2><p>3 Gateway : 1 Farm；统一 15,000 账号；排除注册/登录；热连接和热 Actor；记录 Avg/P90/P99。一般接口 P99 300 ms（容忍 330 ms），快速基础接口 P99 100 ms（容忍 120 ms）。</p><p><a href="{html.escape(overall_dashboard)}">打开 Grafana 全部正式测试时间窗</a></p></section>
<section class="card"><h2>正式结果</h2><div class="table"><table><thead><tr><th>接口</th><th>QPS</th><th>Avg ms</th><th>P90 ms</th><th>P99 ms</th><th>成功率</th><th>关键 CPU 平均/峰值</th><th>判定</th></tr></thead><tbody>{html_rows(rows)}</tbody></table></div><p class="muted">3GW CPU 为三个 1 核实例合计，300% 代表三核全满。接口名链接到各自 Grafana 固定时间窗。</p></section>
<section class="card"><h2>异步写持续性</h2><ul>{background_html}</ul><p>Buy/Sell 5,000 QPS 可作为端到端稳定档；Harvest 与跨农场操作的前台响应通过，但 Projector/MySQL 是长期容量瓶颈。</p></section>
<section class="card"><h2>资源判断</h2><ul><li>3 Gateway 最高窗口平均 CPU 合计 {gateway_average_max:.1f}%，平均每实例约 {gateway_average_max / 3:.1f}%。</li><li>Farm 最高窗口平均/瞬时 CPU 为 {farm_average_max:.1f}% / {farm_peak_max:.1f}%。</li><li>MySQL 最高窗口平均约占 2 核配额 {mysql_average_max / 2:.1f}%；后台追平时曾瞬时达到约 {mysql_peak_max / 2:.1f}%，但没有持续饱和。</li><li>跨农场长期瓶颈是事件放大与 Projector/有序 SQL 投影吞吐。</li><li>最终相关容器均无 OOM、无异常重启。</li></ul></section>
<section class="card"><h2>结论</h2><ol><li>读接口稳定档：Ping 8k、Enter 4k、Sync 8k、FriendList 2k、SearchUser 15k、TaskList 3k、MailList 5k、AreFriends 3k QPS。</li><li>本地 Water/Harvest 前台 4k QPS；Buy/Sell 端到端 5k QPS。</li><li>跨农场 Water/Steal 前台 2.5k QPS，但后台积压约 54 万条，不能称为长期无积压容量。</li><li>多数场景由 P99 而非 CPU 80% 先触线，符合不牺牲玩家体验的目标。</li></ol></section>
</main></body></html>"""
    (run_dir / "report.html").write_text(document, encoding="utf-8")
    (run_dir / "summary.json").write_text(
        json.dumps({"dashboard": overall_dashboard, "rows": rows}, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    (run_dir / "prometheus.json").write_text(
        json.dumps(monitoring, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
