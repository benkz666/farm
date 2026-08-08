#!/usr/bin/env python3
"""汇总三 Gateway、一 Farm 的 A 级接口快速容量测试。"""

import argparse
import html
import json
from pathlib import Path
from typing import Dict, List


def load(path: Path) -> Dict:
    with path.open(encoding="utf-8") as source:
        return json.load(source)


def native(results: Path, filename: str, operation: str, scope: str) -> Dict:
    value = load(results / filename)
    return {
        "operation": operation,
        "scope": scope,
        "throughput": value["actual_qps"],
        "average": value["average_ms"],
        "p95": value["p95_ms"],
        "p99": value["p99_ms"],
        "succeeded": value["succeeded"],
        "failed": value["failed"],
        "kind": "近似吞吐上限",
    }


def metric_value(metric: Dict, key: str, default: float = 0.0) -> float:
    values = metric.get("values", metric)
    return float(values.get(key, default))


def k6_shards(
    results: Path,
    stem: str,
    operation: str,
    scope: str,
    kind: str = "一次性合法状态突发",
) -> Dict:
    summaries = [load(results / f"{stem}-shard-{index}.json") for index in range(3)]
    success = [summary.get("metrics", {}).get("api_operation_success", {}) for summary in summaries]
    latency = [summary.get("metrics", {}).get("api_operation_latency", {}) for summary in summaries]
    counts = [metric_value(value, "count") for value in success]
    total = sum(counts)
    averages = [metric_value(value, "avg") for value in latency]
    weighted_average = sum(avg * count for avg, count in zip(averages, counts)) / total if total else 0
    failed = 0
    for summary in summaries:
        checks = summary.get("metrics", {}).get("checks", {})
        failed += int(metric_value(checks, "fails"))
    return {
        "operation": operation,
        "scope": scope,
        "throughput": sum(metric_value(value, "rate") for value in success),
        "average": weighted_average,
        "p95": max(metric_value(value, "p(95)") for value in latency),
        "p99": max(metric_value(value, "p(99)") for value in latency),
        "succeeded": int(total),
        "failed": failed,
        "kind": kind,
    }


def fmt(value: float, digits: int = 1) -> str:
    return f"{value:,.{digits}f}"


def markdown_table(rows: List[Dict]) -> str:
    lines = [
        "| 接口/场景 | 测试范围 | 吞吐 (QPS) | 平均 (ms) | P95 (ms) | P99 (ms) | 成功/失败 | 结果性质 |",
        "|---|---|---:|---:|---:|---:|---:|---|",
    ]
    for row in rows:
        lines.append(
            f"| {row['operation']} | {row['scope']} | {fmt(row['throughput'])} | "
            f"{fmt(row['average'])} | {fmt(row['p95'])} | {fmt(row['p99'])} | "
            f"{row['succeeded']:,}/{row['failed']:,} | {row['kind']} |"
        )
    return "\n".join(lines)


def html_table(rows: List[Dict]) -> str:
    body = []
    for row in rows:
        cells = [
            row["operation"], row["scope"], fmt(row["throughput"]), fmt(row["average"]),
            fmt(row["p95"]), fmt(row["p99"]), f"{row['succeeded']:,}/{row['failed']:,}", row["kind"],
        ]
        body.append("<tr>" + "".join(f"<td>{html.escape(str(cell))}</td>" for cell in cells) + "</tr>")
    return "".join(body)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--from-ms", required=True)
    parser.add_argument("--to-ms", required=True)
    args = parser.parse_args()

    run_dir = Path(args.run_dir).resolve()
    results = run_dir / "results"
    dashboard = (
        "http://21.6.81.134:33000/d/farm-overview/farm-overview"
        f"?orgId=1&var-cmd=All&var-route=%2Fws&from={args.from_ms}&to={args.to_ms}"
    )

    rows = [
        k6_shards(results, "handshake", "100 Handshake", "前端→3 Gateway", "连接突发上限"),
        native(results, "ping-100000.json", "102 Ping", "前端→3 Gateway"),
        native(results, "enter-60000.json", "200 EnterFarm", "前端→3 Gateway→Farm"),
        native(results, "sync-100000.json", "204 SyncFarm", "前端→3 Gateway→Farm"),
        k6_shards(results, "water", "212 Water（本地）", "前端→3 Gateway→Farm"),
        k6_shards(results, "harvest", "220 Harvest", "前端→3 Gateway→Farm"),
        k6_shards(results, "steal-safe", "222 Steal", "前端→3 Gateway→Farm→Social", "安全吞吐下界"),
        k6_shards(results, "buy", "302 Buy", "前端→3 Gateway→Farm"),
        k6_shards(results, "sell", "304 Sell", "前端→3 Gateway→Farm"),
        native(results, "friend-list-90000.json", "400 FriendList", "前端→3 Gateway→Social"),
        native(results, "search-user-90000.json", "410 SearchUser", "前端→3 Gateway→Social"),
        k6_shards(results, "water-visitor-safe", "212 Water（跨农场）", "前端→3 Gateway→Farm→Social", "安全吞吐下界"),
        native(results, "task-list-60000.json", "600 TaskList", "前端→3 Gateway→Farm"),
        native(results, "mail-list-60000.json", "604 MailList", "前端→3 Gateway→Farm"),
        native(results, "are-friends-60000.json", "AreFriends gRPC", "直连 Social"),
    ]

    monitor = {
        "Farm CPU 峰值": "0.73 核",
        "Farm RSS 峰值": "1000.2 MiB / 1 GiB（触发 OOM）",
        "3 Gateway CPU 峰值合计": "1.43 核 / 3 核",
        "Gateway RSS 峰值": "gateway-0 421.9、gateway-1 419.4、gateway-2 420.0 MiB",
        "Social CPU 峰值": "0.41 核 / 1 核",
        "MySQL 查询峰值": "7,151.5 queries/s",
        "MySQL 运行/连接线程峰值": "24 / 33",
        "Redis 命令峰值": "6,140.8 ops/s",
        "Redis 内存峰值": "286.5 MiB / 1 GiB",
    }
    monitor_md = "\n".join(f"- {key}：{value}" for key, value in monitor.items())

    report = f"""# A 级接口性能测试报告

## 测试口径

- 拓扑：3 × Gateway（各 1 核 1 GiB）+ 1 × Farm（1 核 1 GiB）；Social、MySQL、Redis 均为 1 实例。
- 发压端：k6/servicebench 容器 24 核；三个 Gateway 使用独立账号分片并被直接发压。
- 按要求排除注册和登录。读接口从高 QPS 快速逼近，减少反复边界档；一次性写接口使用独立合法夹具。
- “近似吞吐上限”适合横向比较；一次性状态操作只能给出本轮突发完成速率，不应等同无限时长稳定 QPS。
- [Grafana 固定测试时间范围]({dashboard})

## 接口结果

{markdown_table(rows)}

## 监控峰值

{monitor_md}

## 结论与瓶颈

1. 读链路的最高观测吞吐为 Ping 82,210 QPS；FriendList、SyncFarm、SearchUser 分别约 75,604、75,559、70,441 QPS。三 Gateway 的 CPU 合计峰值仅 1.43 核，说明在该拓扑下 Gateway 已不是唯一硬瓶颈。
2. EnterFarm、TaskList、MailList 落在约 53,000–56,000 QPS。它们进入 Farm，受单 Farm 的 Actor 调度、编码与返回链路约束；预编码和批处理去 HOL 后已明显高于状态写链路。
3. 本地状态写的突发完成速率约 2,133–2,867 QPS。其瓶颈是单账号/Actor 串行、状态变更、任务推进和持久化批次，而不是纯协议转发。
4. Steal 900 账号突发安全通过，观测下界为 949 QPS；6,000 账号同时突发时 Farm RSS 达到 1 GiB 并 OOM。跨农场 Water 安全下界为 1,166 QPS。两者经过 Social 鉴权和多 Actor/推送路径，当前首要硬限制是 Farm 的在途并发内存。
5. MySQL 峰值约 7,152 queries/s，虽出现 24 个 running threads，但本轮先触发的是 Farm OOM；不能据此宣称 MySQL 已到极限。下一轮若专测写链路，应限制在途并发并观察 MySQL CPU、锁等待和磁盘延迟。

## 本轮实施的 1 / 2 / 3

1. 三 Gateway 分片发压：支持显式 Gateway URL 和不重叠账号分片，避免负载只落到一个实例或三个发压器争用同一账号。
2. Farm gRPC 去批次队头阻塞：按 UID 分片保证同用户顺序，完成响应按 50 微秒/64 条聚合发送，慢 UID 不再阻塞同批次快 UID。
3. 热读缓存：FriendList、SearchUser、TaskList、MailList 使用 64 分片缓存；Task/Mail 增加代际屏障和预编码结果，写后精确失效，避免并发回填旧数据。

## 结果解释

- 本报告用于快速定位吞吐量级和低 QPS 接口，并非严格 SLO 认证。
- Handshake 和状态写测试包含大量建连，其 P95/P99 同时反映连接突发与业务响应。
- Steal 的“大突发失败档”被保留为容量证据；报告采用恢复后 900/900 成功档作为安全下界，没有把 OOM 档虚构成最大稳定 QPS。
"""
    (run_dir / "report.md").write_text(report, encoding="utf-8")

    monitor_html = "".join(
        f"<li><strong>{html.escape(key)}</strong>：{html.escape(value)}</li>" for key, value in monitor.items()
    )
    document = f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>A 级接口性能测试报告</title><style>
body{{font:15px/1.6 system-ui,sans-serif;color:#172033;background:#f4f7fb;margin:0}}main{{max-width:1480px;margin:32px auto;padding:0 24px}}
.card{{background:white;border:1px solid #dce3ed;border-radius:12px;padding:22px;margin:16px 0;box-shadow:0 3px 14px #2030500d}}
h1,h2{{color:#12233f}}table{{border-collapse:collapse;width:100%;font-size:13px}}th,td{{padding:10px;border-bottom:1px solid #e6ebf2;text-align:right;white-space:nowrap}}
th:first-child,td:first-child,th:nth-child(2),td:nth-child(2),th:last-child,td:last-child{{text-align:left}}th{{background:#f0f5fb;position:sticky;top:0}}code{{background:#eef2f7;padding:2px 5px;border-radius:4px}}
.note{{color:#526176}}a{{color:#1677c8}}@media(max-width:900px){{.table{{overflow:auto}}}}
</style></head><body><main><h1>A 级接口性能测试报告</h1>
<div class="card"><h2>测试口径</h2><p>3 × Gateway（各 1 核 1 GiB）+ 1 × Farm（1 核 1 GiB），24 核发压端；排除注册、登录。</p><p><a href="{html.escape(dashboard)}">打开 Grafana 固定测试时间范围</a></p></div>
<div class="card"><h2>接口结果</h2><div class="table"><table><thead><tr><th>接口/场景</th><th>范围</th><th>QPS</th><th>平均 ms</th><th>P95 ms</th><th>P99 ms</th><th>成功/失败</th><th>结果性质</th></tr></thead><tbody>{html_table(rows)}</tbody></table></div></div>
<div class="card"><h2>监控峰值</h2><ul>{monitor_html}</ul></div>
<div class="card"><h2>关键结论</h2><ol><li>纯读链路约 53,000–82,000 QPS；三 Gateway 合计 CPU 峰值 1.43 核，当前不是唯一硬瓶颈。</li><li>本地状态写突发约 2,133–2,867 QPS，受 Actor 串行、任务推进与持久化影响。</li><li>Steal 的 6,000 账号突发令单 Farm 达到 1 GiB 并 OOM；900 账号档 100% 成功，安全观测下界 949 QPS。</li><li>MySQL 峰值 7,152 queries/s，但本轮先触发 Farm OOM，不能判定 MySQL 已到极限。</li></ol><p class="note">详细方法、改动和解释请参见同目录 report.md。</p></div>
</main></body></html>"""
    (run_dir / "report.html").write_text(document, encoding="utf-8")
    (run_dir / "summary.json").write_text(
        json.dumps({"dashboard": dashboard, "rows": rows, "monitor": monitor}, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
