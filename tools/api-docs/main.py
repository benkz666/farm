#!/usr/bin/env python3
"""Generate the frontend API catalog, AsyncAPI contract and gRPC reference.

The generator deliberately uses only the Python standard library so contract checks do
not add another runtime dependency. OpenAPI remains hand-authored because the public
HTTP surface is small; WebSocket metadata below is checked against both client and
server constants by tests.
"""

import argparse
import html
import json
import re
from pathlib import Path


COMMANDS = [
    # cmd, name, frontend method, tier, scenario, reason, repeatable
    (100, "Handshake", "handshake", "ladder", "ws_handshake", "连接与会话注册", True),
    (102, "Ping", "internal heartbeat", "ladder", "ws_ping", "高频长连接心跳", True),
    (200, "EnterFarm", "enterFarm", "ladder", "enter_farm", "Actor 加载和缓存回源", True),
    (202, "LeaveFarm", "leaveFarm", "baseline", "leave_farm", "房间订阅清理", True),
    (204, "SyncFarm", "syncFarm", "ladder", "sync_farm", "高频读取和序列化", True),
    (206, "Till", "plotAction", "baseline", "till", "地块状态写", False),
    (208, "Clear", "plotAction", "baseline", "clear", "地块状态写", False),
    (210, "Plant", "plotAction", "baseline", "plant", "背包与地块状态写", False),
    (212, "Water", "plotAction", "ladder", "water_local", "高频状态写，另有跨农场路径", False),
    (214, "RemoveWeed", "plotAction", "baseline", "remove_weed", "地块状态写", False),
    (216, "RemovePest", "plotAction", "baseline", "remove_pest", "地块状态写", False),
    (218, "Fertilize", "plotAction", "baseline", "fertilize", "背包与地块状态写", False),
    (220, "Harvest", "plotAction", "ladder", "harvest", "结算、背包与任务推进", False),
    (222, "Steal", "steal", "ladder", "steal_cross", "跨服务和热点 Actor", False),
    (302, "Buy", "buy", "ladder", "buy", "资产变更和持久化写", False),
    (304, "Sell", "sell", "ladder", "sell", "资产变更和持久化写", False),
    (400, "FriendList", "friendList", "ladder", "friend_list", "Redis 热点和缓存回源", True),
    (402, "GenShareLink", "genShareLink", "baseline", "gen_share_link", "低频社交读", True),
    (404, "AcceptInvite", "acceptInvite", "baseline", "accept_invite", "好友关系写", False),
    (406, "RemoveFriend", "removeFriend", "baseline", "remove_friend", "好友关系写", False),
    (408, "AddFriendByUID", "addFriendByUID", "baseline", "add_friend_by_uid", "好友关系写", False),
    (410, "SearchUser", "searchUser", "ladder", "search_user", "用户数据库读", True),
    (412, "RequestFriend", "requestFriend", "baseline", "request_friend", "好友申请写", False),
    (414, "ListFriendRequests", "listFriendRequests", "baseline", "list_friend_requests", "好友申请读", True),
    (416, "AcceptFriendRequest", "acceptFriendRequest", "baseline", "accept_friend_request", "好友关系事务写", False),
    (418, "RejectFriendRequest", "rejectFriendRequest", "baseline", "reject_friend_request", "好友申请写", False),
    (500, "PetStatus", "petStatus", "baseline", "pet_status", "低频状态读", True),
    (502, "PetActivate", "petActivate", "baseline", "pet_activate", "宠物状态写", False),
    (504, "PetFeed", "petFeed", "baseline", "pet_feed", "背包与宠物状态写", False),
    (600, "TaskList", "taskList", "ladder", "task_list", "任务数据读取", True),
    (602, "TaskClaim", "taskClaim", "baseline", "task_claim", "一次性奖励写", False),
    (604, "MailList", "mailList", "ladder", "mail_list", "邮件列表查询和编码", True),
    (606, "MailRead", "mailReadAll", "baseline", "mail_read", "邮件状态写", False),
    (608, "MailClaim", "mailClaim", "baseline", "mail_claim", "一次性附件领取", False),
    (610, "MailDelete", "mailDeleteAll", "baseline", "mail_delete", "邮件状态写", False),
    (612, "CodexList", "codexList", "baseline", "codex_list", "低频图鉴读", True),
    (614, "ClaimDailyLogin", "claimDailyLogin", "baseline", "claim_daily_login", "一次性奖励写", False),
    (616, "SetTimeProfile", "setTimeProfile", "functional-only", "set_time_profile", "仅开发环境调试", False),
]

PUSHES = [
    (9000, "FarmDelta", "onDelta", "farm_delta", "农场变化广播"),
    (9002, "PlayerDelta", "onPlayerDelta", "player_delta", "玩家资产变化"),
    (9004, "MailNotify", "onMailNotify", "mail_notify", "新邮件或社交提示"),
    (9006, "SessionKick", "internal handler", "session_kick", "同账号新会话踢下线"),
    (9008, "TaskNotify", "onTaskNotify", "task_notify", "任务进度变化"),
]

# Wire request payloads. `u64` is always emitted as DecimalUint64 so the
# generated contract cannot accidentally teach JavaScript clients to use Number.
PAYLOADS = {
    100: [("token", "string"), ("client_config_ver", "integer"), ("resume_farm_uid", "u64"), ("resume_farm_seq", "u64")],
    102: [("client_time", "int64")],
    200: [("owner_uid", "u64")],
    204: [("owner_uid", "u64"), ("from_seq", "u64")],
    206: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    208: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    210: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    212: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    214: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    216: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    218: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    220: [("owner_uid", "u64"), ("plot_index", "integer"), ("arg", "integer")],
    222: [("owner_uid", "u64"), ("plot_index", "integer"), ("crop_id", "integer")],
    302: [("item_id", "integer"), ("quantity", "integer")],
    304: [("item_id", "integer"), ("quantity", "integer")],
    404: [("token", "string")],
    406: [("peer_uid", "u64")],
    408: [("peer_uid", "u64")],
    410: [("username", "string")],
    412: [("peer_uid", "u64")],
    416: [("from_uid", "u64")],
    418: [("from_uid", "u64")],
    502: [("dog_type", "integer")],
    504: [("grams", "integer")],
    602: [("task_id", "integer")],
    606: [("all", "boolean")],
    608: [("mail_id", "u64")],
    610: [("all", "boolean")],
    616: [("time_profile", "string")],
}

HTTP = [
    ("register", "POST /api/register", "register", "ladder", "register"),
    ("login", "POST /api/login", "login", "ladder", "login"),
    ("inviteLanding", "GET /i/{token}", "InviteLanding", "baseline", "invite_landing"),
    ("debugAdvance", "POST /api/debug/advance", "debug controls", "functional-only", "debug_advance"),
]


def catalog():
    operations = []
    for operation_id, endpoint, method, tier, scenario in HTTP:
        operations.append({
            "operationId": operation_id,
            "protocol": "http",
            "endpoint": endpoint,
            "frontendMethod": method,
            "performanceTier": tier,
            "scenario": scenario,
            "repeatable": operation_id in {"login", "inviteLanding"},
            "dataPrerequisite": "预置登录账号" if operation_id == "login" else "合法请求数据",
            "automatedTest": "bench/k6/http_api_baseline.js",
            "currentResult": "not-run",
        })
    for cmd, name, method, tier, scenario, reason, repeatable in COMMANDS:
        operations.append({
            "operationId": name[0].lower() + name[1:],
            "protocol": "websocket",
            "command": cmd,
            "frontendMethod": method,
            "performanceTier": tier,
            "scenario": scenario,
            "reason": reason,
            "repeatable": repeatable,
            "dataPrerequisite": command_prerequisite(cmd),
            "automatedTest": "bench/k6/ws_api_baseline.js",
            "currentResult": "not-run",
        })
    for cmd, name, method, scenario, reason in PUSHES:
        operations.append({
            "operationId": name[0].lower() + name[1:],
            "protocol": "websocket-push",
            "command": cmd,
            "frontendMethod": method,
            "performanceTier": "delivery",
            "scenario": scenario,
            "reason": reason,
            "repeatable": True,
            "dataPrerequisite": "按场景触发服务端推送并保持接收连接在线",
            "automatedTest": push_automated_test(cmd),
            "currentResult": "not-run",
        })
    return {"version": 3, "wireProtocol": "farm.v3.pb", "operations": operations}


def command_prerequisite(cmd):
    if cmd in {100, 102, 202, 400, 402, 414, 500, 600, 604, 612}:
        return "预置有效会话账号"
    if 200 <= cmd <= 222:
        return "预置合法农场、地块和跨农场好友夹具"
    if cmd in {302, 304}:
        return "预置足够金币或可出售库存"
    if 404 <= cmd <= 418:
        return "预置邀请、好友或好友申请关系"
    if 502 <= cmd <= 504:
        return "预置宠物状态和狗粮库存"
    if 602 <= cmd <= 614:
        return "预置可领取任务、邮件、图鉴或登录奖励"
    return "仅开发环境及预置有效会话账号"


def push_automated_test(cmd):
    tests = {
        9000: "go test ./gateway -run TestReceiveFarmDeltaBatchSkipsInvalidConnections",
        9002: "go test ./gateway -run TestVisitorWaterUsesCrossOwnerDecisionAndReceivesDelta",
        9004: "go test ./gateway -run TestGRPCPushMailNotify",
        9006: "go test ./gateway -run TestGatewaySecondLocalSessionKicksFirst",
        9008: "go test ./gateway -run TestSuccessfulHarvestAdvancesTaskAndPublishesTaskNotify",
    }
    return tests[cmd]


def quote(value):
    return json.dumps(value, ensure_ascii=False)


def generate_asyncapi():
    lines = [
        "asyncapi: 3.0.0",
        "info:",
        "  title: 经典农场 WebSocket API",
        "  version: 3.0.0",
        "  description: 浏览器游戏通信契约；仅支持 farm.v3.pb，每帧携带 1~64 个全类型化 Protobuf WireEnvelope，不含 JSON payload 兼容分支。",
        "defaultContentType: application/octet-stream",
        "servers:",
        "  development:",
        "    host: 127.0.0.1:9002",
        "    pathname: /ws",
        "    protocol: ws",
        "channels:",
        "  farm:",
        "    address: /ws",
        "    messages:",
    ]
    for cmd, name, *_ in COMMANDS:
        lines.append(f"      {name}Request:")
        lines.append(f"        $ref: '#/components/messages/{name}Request'")
        lines.append(f"      {name}Response:")
        lines.append(f"        $ref: '#/components/messages/{name}Response'")
    for cmd, name, *_ in PUSHES:
        lines.append(f"      {name}Push:")
        lines.append(f"        $ref: '#/components/messages/{name}Push'")
    lines.append("operations:")
    for cmd, name, _method, tier, scenario, reason, _repeatable in COMMANDS:
        op = name[0].lower() + name[1:]
        lines.extend([
            f"  {op}:",
            "    action: send",
            "    channel: {$ref: '#/channels/farm'}",
            f"    summary: {quote(name + ' 请求')}",
            f"    x-command: {cmd}",
            f"    x-performance-tier: {tier}",
            f"    x-performance-scenario: {scenario}",
            f"    x-performance-reason: {quote(reason)}",
            "    messages:",
            f"      - $ref: '#/channels/farm/messages/{name}Request'",
        ])
        lines.extend([
            f"  receive{name}Response:",
            "    action: receive",
            "    channel: {$ref: '#/channels/farm'}",
            f"    summary: {quote(name + ' 响应')}",
            "    messages:",
            f"      - $ref: '#/channels/farm/messages/{name}Response'",
        ])
    for cmd, name, _method, scenario, reason in PUSHES:
        op = "receive" + name
        lines.extend([
            f"  {op}:",
            "    action: receive",
            "    channel: {$ref: '#/channels/farm'}",
            f"    summary: {quote(name + ' 服务端推送')}",
            f"    x-command: {cmd}",
            "    x-performance-tier: delivery",
            f"    x-performance-scenario: {scenario}",
            f"    x-performance-reason: {quote(reason)}",
            "    messages:",
            f"      - $ref: '#/channels/farm/messages/{name}Push'",
        ])
    lines.extend([
        "components:",
        "  messages:",
    ])
    for cmd, name, *_ in COMMANDS:
        lines.extend(message_yaml(name + "Request", cmd, "request"))
        lines.extend(message_yaml(name + "Response", cmd, "response"))
    for cmd, name, *_ in PUSHES:
        lines.extend(message_yaml(name + "Push", cmd, "push"))
    lines.extend([
        "  schemas:",
        "    DecimalUint64:",
        "      type: string",
        "      pattern: '^[0-9]+$'",
        "      description: 64 位无符号整数的十进制字符串，不得转为 JavaScript Number。",
    ])
    return "\n".join(lines) + "\n"


def message_yaml(message_name, cmd, direction):
    lines = [
        f"    {message_name}:",
        f"      name: {message_name}",
        f"      title: {quote(message_name)}",
        "      correlationId:",
        "        location: $message.payload#/client_seq",
        "      payload:",
        "        type: object",
        "        additionalProperties: false",
        "        required: [cmd, client_seq, err, payload]",
        "        properties:",
        f"          cmd: {{type: integer, const: {cmd}}}",
        "          client_seq:",
        "            type: integer",
        f"            description: {'客户端请求序号' if direction == 'request' else '响应原样回带；推送通常为 0'}",
        f"          err: {{type: integer, format: int32{', const: 0' if direction == 'request' else ''}}}",
        "          payload:",
        "            type: object",
    ]
    fields = PAYLOADS.get(cmd, []) if direction == "request" else []
    if fields:
        lines.append("            additionalProperties: false")
        lines.append("            required: [" + ", ".join(name for name, _ in fields) + "]")
        lines.append("            properties:")
        for field, kind in fields:
            lines.append(f"              {field}:")
            if kind == "u64":
                lines.append("                $ref: '#/components/schemas/DecimalUint64'")
            elif kind == "int64":
                lines.append("                type: integer")
                lines.append("                format: int64")
            else:
                lines.append(f"                type: {kind}")
    else:
        lines.append("            description: 无请求字段，或响应/推送负载由对应命令处理器定义。")
    return lines


def grpc_reference(proto_root):
    sections = []
    for path in sorted(proto_root.rglob("*.proto")):
        source = path.read_text(encoding="utf-8")
        services = re.findall(r"service\s+(\w+)\s*\{([\s\S]*?)\}", source)
        if not services:
            continue
        rendered = [f"<h2>{html.escape(path.name)}</h2>"]
        for service, body in services:
            rendered.append(f"<h3>{html.escape(service)}</h3><table><thead><tr><th>RPC</th><th>请求</th><th>响应</th><th>流</th></tr></thead><tbody>")
            for rpc, request, response_stream, response_type in re.findall(
                r"rpc\s+(\w+)\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)\s*returns\s*\(\s*(stream\s+)?([\w.]+)\s*\)",
                body,
            ):
                stream = "server stream" if response_stream else "unary"
                rendered.append(
                    f"<tr><td>{html.escape(rpc)}</td><td>{html.escape(request)}</td>"
                    f"<td>{html.escape(response_type)}</td><td>{stream}</td></tr>"
                )
            rendered.append("</tbody></table>")
        sections.append("".join(rendered))
    return html_page("经典农场 gRPC API", "".join(sections))


def html_page(title, body):
    return f"""<!doctype html>
<html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\">
<title>{html.escape(title)}</title><style>
body{{font:15px/1.6 system-ui,sans-serif;max-width:1180px;margin:40px auto;padding:0 24px;color:#1f2937}}
a{{color:#087ea4}} table{{width:100%;border-collapse:collapse;margin:12px 0 28px}}th,td{{border:1px solid #d1d5db;padding:8px;text-align:left}}th{{background:#f3f4f6}}
.cards{{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:16px}}.card{{border:1px solid #d1d5db;border-radius:10px;padding:20px}}
</style></head><body><h1>{html.escape(title)}</h1>{body}</body></html>"""


def landing():
    return html_page(
        "经典农场 API 文档",
        """<p>文档仅在开发环境开启。性能等级与自动化场景使用 <code>x-performance-*</code> 扩展关联。</p>
<div class=\"cards\">
<div class=\"card\"><h2>HTTP / OpenAPI</h2><p>注册、登录、邀请与调试接口。</p><a href=\"http/\">打开 Swagger UI</a></div>
<div class=\"card\"><h2>WebSocket / AsyncAPI</h2><p>前端游戏命令、Envelope 与服务端推送。</p><a href=\"ws/\">打开 AsyncAPI</a></div>
<div class=\"card\"><h2>Internal gRPC</h2><p>从 proto 生成的服务与 RPC 索引。</p><a href=\"grpc/\">打开 gRPC 文档</a></div>
<div class=\"card\"><h2>性能覆盖</h2><p>前端方法、命令号、等级和测试场景。</p><a href=\"catalog.json\">查看 JSON 目录</a></div>
</div>""",
    )


def verify_contract(root):
    client_source = (root / "client/src/net/client.js").read_text(encoding="utf-8")
    client_pairs = {
        name: int(value)
        for name, value in re.findall(r"export const CMD_([A-Z0-9_]+)\s*=\s*(\d+)", client_source)
    }
    documented_requests = {cmd for cmd, *_ in COMMANDS}
    documented_pushes = {cmd for cmd, *_ in PUSHES}
    client_requests = {value for value in client_pairs.values() if value < 9000}
    client_pushes = {value for value in client_pairs.values() if value >= 9000}
    if client_requests != documented_requests:
        raise SystemExit("request command drift: client=%s docs=%s" % (sorted(client_requests), sorted(documented_requests)))
    if client_pushes != documented_pushes:
        raise SystemExit("push command drift: client=%s docs=%s" % (sorted(client_pushes), sorted(documented_pushes)))

    benchmark_source = (root / "bench/k6/ws_api_baseline.js").read_text(encoding="utf-8")
    benchmark_commands = {100}
    benchmark_commands.update(
        int(value)
        for value in re.findall(r"\{\s*id:\s*'[^']+',\s*cmd:\s*(\d+)", benchmark_source)
    )
    if benchmark_commands != documented_requests:
        raise SystemExit(
            "request command drift: benchmark=%s docs=%s"
            % (sorted(benchmark_commands), sorted(documented_requests))
        )
    http_benchmark_source = (root / "bench/k6/http_api_baseline.js").read_text(encoding="utf-8")
    benchmark_http = set(re.findall(r"'([A-Za-z][A-Za-z0-9]+)'", re.search(
        r"if \(!\[([^]]+)\]\.includes\(SCENARIO\)\)", http_benchmark_source
    ).group(1)))
    documented_http = {operation_id for operation_id, *_ in HTTP}
    if benchmark_http != documented_http:
        raise SystemExit(
            "HTTP operation drift: benchmark=%s docs=%s"
            % (sorted(benchmark_http), sorted(documented_http))
        )

    gateway_tests = "\n".join(
        path.read_text(encoding="utf-8") for path in (root / "server/gateway").glob("*_test.go")
    )
    for cmd, *_ in PUSHES:
        command = push_automated_test(cmd)
        test_name = re.search(r"-run ([A-Za-z0-9_]+)", command).group(1)
        if not re.search(r"func %s\s*\(" % re.escape(test_name), gateway_tests):
            raise SystemExit("push automated test drift: cmd=%d test=%s" % (cmd, test_name))

    server_source = (root / "server/gateway/envelope.go").read_text(encoding="utf-8")
    server_requests = {
        int(value)
        for value in re.findall(r"Command\w+(?:\s+uint32)?\s*=\s*(\d+)", server_source)
        if int(value) < 9000
    }
    if server_requests != documented_requests:
        raise SystemExit("request command drift: server=%s docs=%s" % (sorted(server_requests), sorted(documented_requests)))

    push_source = (root / "server/shared/clientwire/envelope.go").read_text(encoding="utf-8")
    server_pushes = {int(value) for value in re.findall(r"Command\w+\s+uint32\s*=\s*(\d+)", push_source)}
    if server_pushes != documented_pushes:
        raise SystemExit("push command drift: server=%s docs=%s" % (sorted(server_pushes), sorted(documented_pushes)))

    openapi_source = (root / "docs/api/openapi.yaml").read_text(encoding="utf-8")
    if not re.search(r"DecimalUint64:\s*\n\s+type: string", openapi_source):
        raise SystemExit("OpenAPI DecimalUint64 must remain a string")
    http_source = (root / "server/gateway/http_auth.go").read_text(encoding="utf-8")
    actual_paths = set()
    for path in re.findall(r'mux\.Handle(?:Func)?\("([^\"]+)', http_source):
        if path.startswith("/api/") or path.startswith("/i/"):
            actual_paths.add("/i/{token}" if path == "/i/" else path)
    documented_paths = set(re.findall(r"^  (/[^:]+):$", openapi_source, re.MULTILINE))
    if actual_paths != documented_paths:
        raise SystemExit("HTTP path drift: server=%s docs=%s" % (sorted(actual_paths), sorted(documented_paths)))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve()
    output = args.output.resolve()
    verify_contract(root)
    output.mkdir(parents=True, exist_ok=True)
    (output / "grpc").mkdir(exist_ok=True)
    (output / "ws").mkdir(exist_ok=True)
    (output / "index.html").write_text(landing(), encoding="utf-8")
    (output / "catalog.json").write_text(json.dumps(catalog(), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    asyncapi = generate_asyncapi()
    (output / "asyncapi.yaml").write_text(asyncapi, encoding="utf-8")
    (output / "ws" / "index.html").write_text(
        html_page("WebSocket AsyncAPI", "<p>AsyncAPI HTML 生成器未运行。<a href='../asyncapi.yaml'>查看原始契约</a>。</p>"),
        encoding="utf-8",
    )
    (output / "grpc" / "index.html").write_text(grpc_reference(root / "server/proto"), encoding="utf-8")


if __name__ == "__main__":
    main()
