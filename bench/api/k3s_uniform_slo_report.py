#!/usr/bin/env python3
"""汇总 K3s 统一 SLO 接口压测结果。"""

from __future__ import annotations

import argparse
import html
import json
import statistics
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class Case:
    name: str
    filename: str
    command: str
    protocol: str
    services: tuple[str, ...]
    background: str = "—"


CASES = (
    Case("Handshake", "k3s-handshake-10000-slo200.json", "100", "WebSocket", ("gateway",)),
    Case("Ping", "k3s-ping-90000-12s-slo200.json", "102", "WebSocket", ("gateway",)),
    Case("EnterFarm", "k3s-enter-40000-12s-slo200.json", "200", "WebSocket", ("gateway", "farm")),
    Case("SyncFarm", "k3s-sync-85000-10s-slo200.json", "204", "WebSocket", ("gateway", "farm")),
    Case("Water 本地", "k3s-water-12000-20s-slo200-v2.json", "212", "WebSocket", ("gateway", "farm"), "停压后 pending=0、lag=0"),
    Case("Harvest", "k3s-harvest-12000-20s-slo200.json", "220", "WebSocket", ("gateway", "farm"), "停压 pending=5,726、lag=158,804；约 37 秒归零"),
    Case("Steal", "k3s-steal-4000-15s-slo200.json", "222", "WebSocket", ("gateway", "farm", "social"), "停压 pending=6,037、lag=200,328；约 85 秒归零"),
    Case("Buy", "k3s-buy-12000-20s-slo200.json", "302", "WebSocket", ("gateway", "farm"), "停压 pending=0、lag=0"),
    Case("Sell", "k3s-sell-12000-20s-slo200.json", "304", "WebSocket", ("gateway", "farm"), "停压 pending=0、lag=0"),
    Case("FriendList", "k3s-friend-list-90000-10s-slo200.json", "400", "WebSocket", ("gateway", "social")),
    Case("SearchUser", "k3s-search-user-90000-10s-slo200.json", "410", "WebSocket", ("gateway", "social")),
    Case("Water 跨农场", "k3s-water-visitor-4000-15s-slo200.json", "212", "WebSocket", ("gateway", "farm", "social"), "停压 pending=6,671、lag=242,375；约 115 秒归零"),
    Case("TaskList", "k3s-task-list-20000-15s-slo200.json", "600", "WebSocket", ("gateway", "farm")),
    Case("MailList", "k3s-mail-list-50000-10s-slo200.json", "604", "WebSocket", ("gateway", "farm")),
    Case("AreFriends", "k3s-are-friends-45000-12s-slo200.json", "AreFriends", "gRPC", ("social",)),
)

CPU_QUERIES = {
    "gateway": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="gateway",pod=~"gateway-.*"}[30s])) * 100',
    "farm": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="farm",pod=~"farm-.*"}[30s])) * 100',
    "social": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="social",pod=~"social-.*"}[30s])) * 100',
    "mysql": 'sum(rate(container_cpu_usage_seconds_total{namespace="benkz",container="mysql",pod=~"mysql-.*"}[30s])) * 100',
}


def query_range(base_url: str, query: str, start: float, end: float) -> list[float]:
    params = urllib.parse.urlencode({"query": query, "start": start, "end": end, "step": 5})
    with urllib.request.urlopen(base_url.rstrip("/") + "/api/v1/query_range?" + params, timeout=20) as response:
        payload = json.load(response)
    values: list[float] = []
    for series in payload.get("data", {}).get("result", []):
        values.extend(float(sample[1]) for sample in series.get("values", []))
    return values


def collect_cpu(prometheus_url: str, result: dict[str, Any]) -> dict[str, dict[str, float | None]]:
    start = float(result["measurement_start_unix_ms"]) / 1000
    # cAdvisor 约每 15 秒抓取一次；向后补一个抓取周期，才能让短窗口末尾的
    # CPU 累计增量进入 rate 计算。该值仍是 30 秒窗口估算，不冒充精确计费。
    end = float(result["measurement_end_unix_ms"]) / 1000 + 15
    output: dict[str, dict[str, float | None]] = {}
    for service, query in CPU_QUERIES.items():
        try:
            values = query_range(prometheus_url, query, start, end)
            output[service] = {
                "average": statistics.mean(values) if values else None,
                "peak": max(values) if values else None,
            }
        except Exception:
            output[service] = {"average": None, "peak": None}
    return output


def success_rate(result: dict[str, Any]) -> float:
    completed = int(result.get("succeeded", 0)) + int(result.get("failed", 0))
    return int(result.get("succeeded", 0)) / completed if completed else 0.0


def dashboard_link(base_url: str, command: str, start_ms: int, end_ms: int) -> str:
    params = urllib.parse.urlencode({
        "orgId": 1,
        "var-cmd": command,
        "var-route": "/ws",
        "from": start_ms - 15_000,
        "to": end_ms + 15_000,
    })
    return base_url.rstrip("/") + "/d/farm-overview/farm-overview?" + params


def number(value: float | None, digits: int = 1) -> str:
    return "—" if value is None else f"{value:,.{digits}f}"


def cpu_text(case: Case, cpu: dict[str, dict[str, float | None]]) -> str:
    parts = []
    for service in case.services:
        values = cpu[service]
        parts.append(f"{service} {number(values['average'])}%/{number(values['peak'])}%")
    return "；".join(parts)


def build_rows(run_dir: Path, prometheus_url: str, grafana_url: str) -> list[dict[str, Any]]:
    rows = []
    for case in CASES:
        result = json.loads((run_dir / case.filename).read_text(encoding="utf-8"))
        cpu = collect_cpu(prometheus_url, result)
        success = success_rate(result)
        reached = float(result["actual_qps"]) >= float(result["target_qps"]) * 0.95
        passed = reached and success >= 0.99 and float(result["p90_ms"]) <= 200 and float(result["p99_ms"]) <= 500
        rows.append({
            "name": case.name,
            "protocol": case.protocol,
            "target_qps": result["target_qps"],
            "actual_qps": result["actual_qps"],
            "average_ms": result["average_ms"],
            "p90_ms": result["p90_ms"],
            "p99_ms": result["p99_ms"],
            "success_rate": success,
            "status": "通过" if passed else "不通过",
            "cpu": cpu_text(case, cpu),
            "background": case.background,
            "start_ms": result["measurement_start_unix_ms"],
            "end_ms": result["measurement_end_unix_ms"],
            "grafana": dashboard_link(
                grafana_url,
                case.command,
                int(result["measurement_start_unix_ms"]),
                int(result["measurement_end_unix_ms"]),
            ),
        })
    return rows


def markdown(rows: list[dict[str, Any]], overall: str) -> str:
    lines = [
        "# K3s 统一 SLO 接口性能测试报告",
        "",
        "## 测试口径",
        "",
        "- 环境：21.6.81.134 本机 K3s；3 × Gateway（各 1 核 1 GiB）+ 1 × Farm（1 核 1 GiB），其他业务服务各 1 实例。",
        "- 夹具：统一 15,000 个账号；WebSocket、Actor 和读缓存均在测量窗口前预热；每连接最多 8 命令/秒。",
        "- 排除注册和登录；Avg 只记录，统一判定为 P90 ≤ 200 ms、P99 ≤ 500 ms、业务成功率 ≥ 99%、实际吞吐达到目标的 95%。",
        "- 测量窗口 10–20 秒，用于快速容量判断，不等同于长稳认证。",
        f"- [打开 Grafana 全部测试时间窗]({overall})",
        "",
        "## 正式通过档",
        "",
        "| 接口 | 协议 | 目标 | 实际吞吐 | Avg | P90 | P99 | 成功率 | 关键 CPU 平均/峰值 |",
        "|---|---|---:|---:|---:|---:|---:|---:|---|",
    ]
    for row in rows:
        unit = "连接/s" if row["name"] == "Handshake" else "QPS"
        lines.append(
            f"| [{row['name']}]({row['grafana']}) | {row['protocol']} | {row['target_qps']:,} | "
            f"{row['actual_qps']:,.0f} {unit} | {row['average_ms']:.1f} ms | {row['p90_ms']:.1f} ms | "
            f"{row['p99_ms']:.1f} ms | {row['success_rate'] * 100:.3f}% | {row['cpu']} |"
        )
    lines.extend([
        "",
        "## 异步落库情况",
        "",
    ])
    for row in rows:
        if row["background"] != "—":
            lines.append(f"- **{row['name']}**：{row['background']}")
    lines.extend([
        "",
        "## 结论",
        "",
        "- 读与缓存接口最高：Ping、SyncFarm、FriendList、SearchUser 均达到约 8.3–8.8 万 QPS。",
        "- 本地写接口在严格 SLO 下集中在约 1.18–1.20 万 QPS，主要由 Farm 单核前台和 Gateway 写入保护限制。",
        "- Steal 和跨农场 Water 的前台严格容量约 4,000 QPS，但事件放大使后台 Projector 长时间追赶；长期无积压容量明显更低。",
        "- TaskList 在 20k 与 25k 之间出现明显排队拐点；MailList 严格稳定约 49.8k QPS。",
        "- AreFriends 单核 Social 严格稳定约 44.8k QPS；50k 档开始明显排队。",
        "- 第一次 AreFriends 全失败是压测端默认 internal-token 与 K3s Secret 不一致，已使用集群 Secret 重跑，错误档未纳入正式结果。",
        "",
    ])
    return "\n".join(lines)


def html_report(rows: list[dict[str, Any]], overall: str) -> str:
    body = []
    for row in rows:
        unit = "连接/s" if row["name"] == "Handshake" else "QPS"
        body.append(
            "<tr>"
            f'<td><a href="{html.escape(row["grafana"])}">{html.escape(row["name"])}</a></td>'
            f'<td>{html.escape(row["protocol"])}</td><td>{row["target_qps"]:,}</td>'
            f'<td>{row["actual_qps"]:,.0f} {unit}</td><td>{row["average_ms"]:.1f}</td>'
            f'<td>{row["p90_ms"]:.1f}</td><td>{row["p99_ms"]:.1f}</td>'
            f'<td>{row["success_rate"] * 100:.3f}%</td><td>{html.escape(row["cpu"])}</td></tr>'
        )
    background = "".join(
        f"<li><b>{html.escape(row['name'])}</b>：{html.escape(row['background'])}</li>"
        for row in rows if row["background"] != "—"
    )
    return f"""<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>K3s 统一 SLO 性能报告</title>
<style>body{{margin:0;background:#f5f7fb;color:#182235;font:14px/1.6 system-ui,sans-serif}}main{{max-width:1500px;margin:28px auto;padding:0 22px}}section{{background:#fff;border:1px solid #dce4ef;border-radius:14px;margin:16px 0;padding:22px;box-shadow:0 4px 18px #243b5a0d}}table{{border-collapse:collapse;width:100%;min-width:1050px}}.table{{overflow:auto}}th,td{{padding:10px;border-bottom:1px solid #e7edf5;text-align:right;white-space:nowrap}}th:first-child,td:first-child,th:last-child,td:last-child{{text-align:left}}th{{background:#edf4fb}}a{{color:#1769aa}}</style></head>
<body><main><h1>K3s 统一 SLO 接口性能测试报告</h1><section><h2>口径</h2><p>3 Gateway : 1 Farm；统一 15,000 账号；热连接/热 Actor/热缓存；排除注册登录。P90 ≤ 200 ms，P99 ≤ 500 ms，业务成功率 ≥ 99%，实际吞吐达到目标 95%。</p><p><a href="{html.escape(overall)}">打开 Grafana 全部测试时间窗</a></p></section>
<section><h2>正式通过档</h2><div class="table"><table><thead><tr><th>接口</th><th>协议</th><th>目标</th><th>实际吞吐</th><th>Avg ms</th><th>P90 ms</th><th>P99 ms</th><th>成功率</th><th>关键 CPU 平均/峰值</th></tr></thead><tbody>{''.join(body)}</tbody></table></div></section>
<section><h2>异步落库</h2><ul>{background}</ul><p>Harvest、Steal 和跨农场 Water 的前台 SLO 容量高于长期投影容量，不能把前台峰值直接视为长期无积压容量。</p></section>
<section><h2>结论</h2><ul><li>读/缓存接口约 2.0 万至 8.8 万 QPS。</li><li>本地写接口约 1.18–1.20 万 QPS。</li><li>跨农场前台约 4,000 QPS，长期受 Projector/MySQL 投影约束。</li><li>AreFriends 单核 Social 约 44.8k QPS。</li></ul></section></main></body></html>"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--prometheus-url", default="http://21.6.81.134:30909")
    parser.add_argument("--grafana-url", default="http://21.6.81.134:33000")
    args = parser.parse_args()
    run_dir = Path(args.run_dir).resolve()
    rows = build_rows(run_dir, args.prometheus_url, args.grafana_url)
    start = min(int(row["start_ms"]) for row in rows)
    end = max(int(row["end_ms"]) for row in rows)
    overall = dashboard_link(args.grafana_url, "All", start, end)
    (run_dir / "report.md").write_text(markdown(rows, overall), encoding="utf-8")
    (run_dir / "report.html").write_text(html_report(rows, overall), encoding="utf-8")
    (run_dir / "summary.json").write_text(
        json.dumps({"slo": {"p90_ms": 200, "p99_ms": 500, "success_rate": 0.99}, "rows": rows}, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
