#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
经典农场 · 容量估算与压测通过线推导脚本

用途：把 3000w DAU 的容量推导跑成可复现的计算，输出直接回填进
docs/design/capacity-and-benchmark.md 与 architecture.md 第 2 章。

本脚本与 docs/design/verify-balance.py 是同一层级的东西：
verify-balance.py 为策划数值背书，本脚本为架构容量数字背书。
架构文档里出现的每一个容量数字都必须能在这里找到出处。

单用户数据量的拆解表与 architecture.md 5.8 节严格对应，
任一处改动都要同步另一处并重跑本脚本。

运行：python3 docs/design/capacity-model.py
退出码：0 = 全部断言通过，1 = 存在断言失败
"""

import math
import unicodedata

# ============================================================================
# 配置区 A · 需求侧假设
# 这些是「假设」而不是「事实」，比数字本身更需要被审视。
# 每一条都标注了它的来源，以及它错了会导致什么后果。
# ============================================================================

DAU = 30_000_000              # 题目给定
SESSIONS_PER_DAY = 4          # [假设] 人均日登录次数。农场是碎片化玩法，早中晚各一次 + 一次随机
SESSION_MINUTES = 5           # [假设] 单次会话时长。收菜 + 逛好友的典型时长
OPS_PER_SESSION = 30          # [假设] 单次会话的操作数。见「检查 2」中与作物循环的交叉验证
PEAK_CCU_FACTOR = 4.0         # [假设] 晚高峰并发 / 全天平均并发
PEAK_QPS_FACTOR = 5.0         # [假设] 晚高峰 QPS / 全天平均 QPS，略高于并发系数（高峰期操作更密集）
WRITE_RATIO = 0.40            # [假设] 写操作占比。种植循环的动作绝大多数是写
AVG_ROOM_SUBSCRIBERS = 1.3    # [假设] 农场房间的平均订阅者数。绝大多数时候只有主人自己
DESIGN_HEADROOM = 1.20        # [设计] 设计值相对推导值的余量

REGISTERED_MULTIPLIER = 4     # [假设] 注册用户数 / DAU

# ============================================================================
# 配置区 B · 单用户数据量
# 与 architecture.md 5.8 节的拆解表逐项对应
# ============================================================================

# 内存热数据（Actor 驻留态）
MEM_CORE = 128                      # uid / 昵称 / 等级 / 经验 / 金币 / 时间戳
MEM_PLOTS = 18 * 112                # 18 块地 x sizeof(Plot)
MEM_FRIENDS = 200 * 8               # 好友视图，200 上限时的上界
MEM_ITEMS = 29 * 12 + 32 * 12       # 仓库 29 种果实 + 背包 32 种道具
MEM_DAILY = 64                      # DailyState
MEM_PET = 64                        # PetState
MEM_CODEX = 8                       # 图鉴 bitmap
MEM_MAIL_SUMMARY = 20 * 128         # 邮件摘要，正文按需拉取
GO_OVERHEAD = 1.4                   # [假设] 对象头 / slice header / map 桶 / 内存对齐

# 落库冷数据（同上，但邮件存正文而非摘要）
DB_MAIL_FULL = 20 * 300             # 邮件正文均值
MYSQL_ROW_OVERHEAD = 2.5            # [假设] 行头 + 索引 + 页填充率

OPLOG_RATIO = 0.10                  # [假设] 需要记流水的关键操作占全部操作的比例
OPLOG_BYTES = 100                   # [假设] 单条流水字节数
OPLOG_RETAIN_DAYS = 7               # [设计] 在线流水保留天数

# ============================================================================
# 配置区 C · 单机能力假设
# 这些数字必须由压测实测替换。当前值是保守的行业经验值，
# 「检查 10」会反推出压测必须达到的通过线。
# ============================================================================

CONN_PER_GATEWAY = 250_000          # [假设] 单台网关承载的 WebSocket 连接数
BYTES_PER_CONN = 4_608              # [假设] 读 buffer 2K + 写 buffer 2K + conn 结构 512B
GATEWAY_RAM_GB = 32
GATEWAY_NIC_GBPS = 10
GATEWAY_REDUNDANCY = 1.5            # [设计] N+50%

QPS_PER_FARMSERVER = 30_000         # [假设] 单台逻辑服的 QPS 上限，压测须验证
FARMSERVER_RAM_GB = 64
FARMSERVER_REDUNDANCY = 1.5
ACTOR_VISIT_MULTIPLIER = 1.3        # [假设] 活跃 Actor / PCU，含被访问但主人离线的农场

REDIS_BYTES_PER_USER = 8 * 1024     # [假设] Redis 编码比 Go 内存紧凑
REDIS_INSTANCE_GB = 16
REDIS_INSTANCES_PER_HOST = 2        # 单机多实例可利用多核，但要给 fork 留出内存
REDIS_HOST_RAM_GB = 64

WRITE_MERGE_RATIO = 5.0             # [假设] write-behind 合并率，压测须验证
MYSQL_SAFE_WRITE_TPS = 3_000        # [假设] 单实例安全写 TPS
MYSQL_SAFE_GB_PER_INSTANCE = 500    # [假设] 单实例舒适容量
MYSQL_GROWTH_HEADROOM = 2.0         # [设计] 分片数的增长余量

BYTES_PER_MSG_OUT = 250             # [假设] 单条出向消息 protobuf 编码后大小
BYTES_PER_MSG_IN = 120

OTHER_MACHINES = 26                 # Auth 4 + Social 4 + Task 4 + Mail 4 + Kafka 6 + 运维 4

EXTRAPOLATION_DISCOUNT = 0.65       # [设计] 单机实测外推到集群的折扣系数

# ============================================================================
# 配置区 D · 压测参数
# 引自 game-design-full.md 第 3 章时间系统与第 6 章作物表
# ============================================================================

TIME_SCALE_BENCH = 1 / 3600         # [策划 3.2] bench 档
LOGIC_DAY_MIN_SECONDS = 300         # [策划 3.4] 逻辑日真实时长下限 5 分钟
RADISH_HOURS = 10                   # [策划 6.2] 白萝卜成熟时长（缩放小时）
INITIAL_PLOTS = 6                   # [策划 4.2] 初始地块数
WATERINGS_PER_SEASON = 3            # [策划 7.2] 水分持续 35% -> 每季约 3 次浇水
HAZARDS_PER_SEASON = 10 * (0.12 + 0.10)  # [策划 7.2] 10 窗口 x (长草 12% + 生虫 10%)
FIXED_OPS_PER_CYCLE = 4             # 锄地 + 播种 + 收获 + 清理
SOCIAL_OPS_MULTIPLIER = 1.3         # [假设] 社交操作（偷菜/互助/商店）带来的额外操作占比

# ============================================================================
# 显示宽度对齐（CJK 字符占两列）
# ============================================================================


def dw(s):
    return sum(2 if unicodedata.east_asian_width(str(c)) in "FW" else 1 for c in str(s))


def L(s, w):
    return str(s) + " " * max(0, w - dw(s))


def R(s, w):
    return " " * max(0, w - dw(s)) + str(s)


def next_pow2(n):
    return 1 << math.ceil(math.log2(n))


def gb(b):
    return b / 1024 ** 3


def tb(b):
    return b / 1024 ** 4


def wan(n):
    """以「万」为单位显示，中文语境下比科学计数法直观。"""
    return f"{n / 10_000:,.1f} 万"


FAILURES = []


def rule(title):
    print("\n" + "=" * 100)
    print(title)
    print("=" * 100)


def assert_that(cond, msg):
    if not cond:
        FAILURES.append(msg)
        print(f"  [!] 断言失败: {msg}")


def kv(label, value, note=""):
    print(L(label, 34) + R(value, 20) + ("   " + note if note else ""))


# ============================================================================
# 检查 1 · 在线数推导
# ============================================================================

def check_concurrency():
    rule("检查 1 · 在线数推导")
    total_minutes = DAU * SESSIONS_PER_DAY * SESSION_MINUTES
    avg_ccu = total_minutes / (24 * 60)
    pcu = avg_ccu * PEAK_CCU_FACTOR
    pcu_design = pcu * DESIGN_HEADROOM

    print("假设：人均日登录 %d 次，单次 %d 分钟，晚高峰系数 %.1f\n"
          % (SESSIONS_PER_DAY, SESSION_MINUTES, PEAK_CCU_FACTOR))
    kv("日总在线时长", f"{total_minutes / 60:,.0f} 小时")
    kv("全天平均并发", wan(avg_ccu))
    kv("峰值并发 PCU", wan(pcu))
    kv("PCU 设计值", wan(pcu_design), f"= 推导值 x {DESIGN_HEADROOM}")

    ratio = pcu / DAU
    print()
    kv("PCU / DAU", f"{ratio * 100:.1f}%", "休闲游戏典型区间 3% — 20%")
    assert_that(0.03 <= ratio <= 0.20,
                f"PCU/DAU = {ratio:.1%} 落在休闲游戏合理区间之外，需重新审视会话假设")
    return avg_ccu, pcu, pcu_design


# ============================================================================
# 检查 2 · 请求量与消息量推导
# ============================================================================

def check_qps(pcu, pcu_design):
    rule("检查 2 · 请求量与出向消息量推导")
    daily_req = DAU * SESSIONS_PER_DAY * OPS_PER_SESSION
    avg_qps = daily_req / 86400
    peak_qps = avg_qps * PEAK_QPS_FACTOR
    peak_qps_design = peak_qps * DESIGN_HEADROOM
    peak_write = peak_qps * WRITE_RATIO
    peak_write_design = peak_qps_design * WRITE_RATIO

    print("假设：人均日操作 %d 次，峰值系数 %.1f，写占比 %.0f%%\n"
          % (SESSIONS_PER_DAY * OPS_PER_SESSION, PEAK_QPS_FACTOR, WRITE_RATIO * 100))
    kv("日请求总数", f"{daily_req / 1e8:,.1f} 亿")
    kv("全天平均 QPS", wan(avg_qps))
    kv("峰值 QPS", wan(peak_qps))
    kv("峰值 QPS 设计值", wan(peak_qps_design), f"= 推导值 x {DESIGN_HEADROOM}")
    kv("峰值写 QPS", wan(peak_write))

    # 出向消息 = 每个请求的应答 + 每次写操作向房间订阅者的广播
    out_rsp = peak_qps
    out_bcast = peak_write * AVG_ROOM_SUBSCRIBERS
    out_total = out_rsp + out_bcast
    print()
    kv("出向 · 请求应答", f"{wan(out_rsp)} msg/s")
    kv("出向 · 房间广播", f"{wan(out_bcast)} msg/s", f"扇出 {AVG_ROOM_SUBSCRIBERS}")
    kv("出向合计", f"{wan(out_total)} msg/s")

    # 交叉验证：人均操作频率是否符合直觉
    ops_per_sec_per_user = peak_qps / pcu
    print()
    kv("峰值人均操作频率", f"{ops_per_sec_per_user:.3f} 次/秒",
       f"即每 {1 / ops_per_sec_per_user:.0f} 秒一次操作")
    assert_that(0.05 <= ops_per_sec_per_user <= 0.5,
                f"人均操作频率 {ops_per_sec_per_user:.3f}/s 不合常理，"
                f"OPS_PER_SESSION 与 SESSION_MINUTES 两个假设互相矛盾")
    return peak_qps, peak_qps_design, peak_write, peak_write_design, out_total


# ============================================================================
# 检查 3 · 单用户数据量拆解
# ============================================================================

def check_per_user_bytes():
    rule("检查 3 · 单用户数据量拆解（对应 architecture.md 5.8 节）")
    items = [
        ("玩家核心字段", MEM_CORE, "uid / 昵称 / 等级 / 经验 / 金币"),
        ("地块", MEM_PLOTS, "18 x 112 B"),
        ("好友视图", MEM_FRIENDS, "200 x 8 B，满好友上界"),
        ("仓库 + 背包", MEM_ITEMS, "29 种果实 + 32 种道具"),
        ("每日状态", MEM_DAILY, "DailyState"),
        ("宠物", MEM_PET, "PetState"),
        ("图鉴", MEM_CODEX, "29 位 bitmap"),
        ("邮件摘要", MEM_MAIL_SUMMARY, "20 封 x 128 B"),
    ]
    print(L("组成", 22) + R("字节", 10) + "   说明")
    print("-" * 100)
    subtotal = 0
    for name, b, note in items:
        print(L(name, 22) + R(f"{b:,}", 10) + "   " + note)
        subtotal += b
    print("-" * 100)
    print(L("小计", 22) + R(f"{subtotal:,}", 10))
    mem_per_user = subtotal * GO_OVERHEAD
    print(L(f"Go 运行时开销 x{GO_OVERHEAD}", 22) + R(f"{mem_per_user:,.0f}", 10))
    print()
    kv("内存热数据 / 用户", f"{mem_per_user / 1024:.1f} KB")

    db_per_user = subtotal - MEM_MAIL_SUMMARY + DB_MAIL_FULL
    kv("落库裸数据 / 用户", f"{db_per_user / 1024:.1f} KB", "邮件按正文计，不含行开销")

    assert_that(abs(mem_per_user / 1024 - 10.0) < 1.0,
                f"内存热数据 {mem_per_user / 1024:.1f} KB 与文档声明的 10 KB 基数偏离过大")
    return mem_per_user, db_per_user


# ============================================================================
# 检查 4 · 存储总量与 MySQL 分片数
# ============================================================================

def check_storage(db_per_user, peak_write_design, daily_req):
    rule("检查 4 · 存储总量与 MySQL 分片数")
    registered = DAU * REGISTERED_MULTIPLIER
    raw = registered * db_per_user
    with_overhead = raw * MYSQL_ROW_OVERHEAD
    oplog = daily_req * OPLOG_RATIO * OPLOG_BYTES * OPLOG_RETAIN_DAYS
    total = with_overhead + oplog

    kv("注册用户数", f"{registered / 1e8:.1f} 亿", f"= DAU x {REGISTERED_MULTIPLIER}")
    kv("裸数据", f"{tb(raw):.2f} TB")
    kv(f"含行开销与索引 x{MYSQL_ROW_OVERHEAD}", f"{tb(with_overhead):.2f} TB")
    kv(f"在线流水（保留 {OPLOG_RETAIN_DAYS} 天）", f"{tb(oplog):.2f} TB")
    kv("在线总存储", f"{tb(total):.2f} TB")

    print()
    db_tps = peak_write_design / WRITE_MERGE_RATIO
    by_tps = math.ceil(db_tps / MYSQL_SAFE_WRITE_TPS)
    by_size = math.ceil(gb(total) / MYSQL_SAFE_GB_PER_INSTANCE)
    needed = max(by_tps, by_size)
    shards = next_pow2(needed * MYSQL_GROWTH_HEADROOM)

    kv("峰值写 QPS（设计值）", wan(peak_write_design))
    kv(f"write-behind 合并 {WRITE_MERGE_RATIO}:1 后", f"{db_tps:,.0f} TPS")
    kv("按写 TPS 需要分片", f"{by_tps} 个", f"单实例安全 {MYSQL_SAFE_WRITE_TPS} TPS")
    kv("按容量需要分片", f"{by_size} 个", f"单实例 {MYSQL_SAFE_GB_PER_INSTANCE} GB")
    kv("取大者并留增长余量", f"{shards} 个主分片", f"向上取 2 的幂，x{MYSQL_GROWTH_HEADROOM} 余量")
    kv("逻辑分片 / 物理分片", f"1024 / {shards} = {1024 // shards}", "每个物理实例承载的逻辑分片数")

    assert_that(shards * MYSQL_SAFE_WRITE_TPS >= db_tps,
                "分片总写能力不足以承载 write-behind 后的 TPS")
    assert_that(shards * MYSQL_SAFE_GB_PER_INSTANCE >= gb(total),
                "分片总容量不足以承载在线数据")
    assert_that(1024 % shards == 0,
                f"物理分片数 {shards} 不能整除 1024 个逻辑分片，扩容时分配不均")
    return total, shards


# ============================================================================
# 检查 5 · 接入层容量
# ============================================================================

def check_gateway(pcu_design, out_total):
    rule("检查 5 · 接入层容量（Gateway）")
    needed = math.ceil(pcu_design / CONN_PER_GATEWAY)
    machines = math.ceil(needed * GATEWAY_REDUNDANCY)
    conn_mem = CONN_PER_GATEWAY * BYTES_PER_CONN
    mem_ratio = gb(conn_mem) / GATEWAY_RAM_GB

    kv("PCU 设计值", wan(pcu_design))
    kv("单机连接数", f"{CONN_PER_GATEWAY:,}")
    kv("裸需求", f"{needed} 台")
    kv(f"含冗余 x{GATEWAY_REDUNDANCY}", f"{machines} 台")
    print()
    kv("单机连接内存", f"{gb(conn_mem):.2f} GB", f"{BYTES_PER_CONN} B/连接")
    kv("占单机内存", f"{mem_ratio * 100:.1f}%", f"机器规格 {GATEWAY_RAM_GB} GB")

    out_bps = out_total * BYTES_PER_MSG_OUT * 8
    in_bps = out_total / (1 + AVG_ROOM_SUBSCRIBERS * WRITE_RATIO) * BYTES_PER_MSG_IN * 8
    nic_ratio = (out_bps / machines) / (GATEWAY_NIC_GBPS * 1e9)
    print()
    kv("集群出向带宽", f"{out_bps / 1e9:.2f} Gbps")
    kv("集群入向带宽", f"{in_bps / 1e9:.2f} Gbps")
    kv("单机出向占网卡", f"{nic_ratio * 100:.1f}%", f"网卡 {GATEWAY_NIC_GBPS} Gbps")

    assert_that(mem_ratio <= 0.70,
                f"单机连接内存占比 {mem_ratio:.1%} 超过 70%，连接数或 buffer 需要下调")
    assert_that(nic_ratio <= 0.30,
                f"单机出向带宽占网卡 {nic_ratio:.1%} 超过 30%，突发流量会打满网卡")
    return machines, out_bps


# ============================================================================
# 检查 6 · 逻辑层容量
# ============================================================================

def check_farmserver(pcu_design, peak_qps_design, mem_per_user):
    rule("检查 6 · 逻辑层容量（FarmServer）")
    needed = math.ceil(peak_qps_design / QPS_PER_FARMSERVER)
    with_redundancy = math.ceil(needed * FARMSERVER_REDUNDANCY)
    # 取 2 的幂，使 1024 个逻辑分片能均匀分配到每个实例
    machines = next_pow2(with_redundancy)
    actors = pcu_design * ACTOR_VISIT_MULTIPLIER
    actor_mem = actors * mem_per_user
    per_machine = actor_mem / machines
    mem_ratio = gb(per_machine) / FARMSERVER_RAM_GB

    kv("峰值 QPS 设计值", wan(peak_qps_design))
    kv("单机 QPS 假设", f"{QPS_PER_FARMSERVER:,}", "压测须验证，见检查 10")
    kv("裸需求", f"{needed} 台")
    kv(f"含冗余 x{FARMSERVER_REDUNDANCY}", f"{with_redundancy} 台")
    kv("向上取 2 的幂", f"{machines} 台", f"1024 / {machines} = {1024 // machines} 个逻辑分片每实例")
    print()
    kv("活跃 Actor 数", wan(actors), f"= PCU x {ACTOR_VISIT_MULTIPLIER}")
    kv("Actor 总内存", f"{gb(actor_mem):.1f} GB")
    kv("单机 Actor 内存", f"{gb(per_machine):.2f} GB")
    kv("占单机内存", f"{mem_ratio * 100:.1f}%", f"机器规格 {FARMSERVER_RAM_GB} GB")

    print("\n注：Actor 内存远未成为瓶颈，逻辑层的约束是 CPU 而非内存。")
    print("    这是惰性推进（决策 D1）的直接结果——不驻留的农场完全不占资源。")

    assert_that(mem_ratio <= 0.50,
                f"单机 Actor 内存占比 {mem_ratio:.1%} 超过 50%，"
                f"GC 压力会显著上升")
    assert_that(1024 % machines == 0,
                f"FarmServer 台数 {machines} 不能整除 1024 个逻辑分片，负载会不均")
    return machines


# ============================================================================
# 检查 7 · 缓存层容量
# ============================================================================

def check_redis():
    rule("检查 7 · 缓存层容量（Redis）")
    cache = DAU * REDIS_BYTES_PER_USER
    masters = math.ceil(gb(cache) / REDIS_INSTANCE_GB)
    instances = masters * 2
    hosts = math.ceil(masters / REDIS_INSTANCES_PER_HOST) * 2

    kv("缓存目标", "DAU 全量", f"{DAU / 1e4:,.0f} 万用户")
    kv("单用户缓存", f"{REDIS_BYTES_PER_USER / 1024:.0f} KB")
    kv("缓存总量", f"{gb(cache):.1f} GB")
    kv("主实例数", f"{masters} 个", f"单实例 {REDIS_INSTANCE_GB} GB")
    kv("含从库总实例", f"{instances} 个")
    kv("物理机", f"{hosts} 台", f"每台 {REDIS_INSTANCES_PER_HOST} 实例，主从分离部署")

    host_ratio = REDIS_INSTANCES_PER_HOST * REDIS_INSTANCE_GB / REDIS_HOST_RAM_GB
    kv("单机内存占用", f"{host_ratio * 100:.0f}%", f"机器规格 {REDIS_HOST_RAM_GB} GB")

    assert_that(masters * REDIS_INSTANCE_GB >= gb(cache),
                "Redis 容量不足以缓存 DAU 全量")
    assert_that(host_ratio <= 0.60,
                f"Redis 单机内存占用 {host_ratio:.0%} 超过 60%，"
                f"RDB fork 时的写时复制会触发 OOM")
    return hosts


# ============================================================================
# 检查 8 · 机器清单汇总
# ============================================================================

def check_machines(gw, fs, redis_hosts, shards):
    rule("检查 8 · 核心集群机器清单")
    mysql_hosts = shards * 2
    rows = [
        ("Gateway", gw, "16C32G", "无状态，按连接数扩容"),
        ("FarmServer", fs, "16C64G", "有状态，按逻辑分片路由"),
        ("Redis", redis_hosts, "16C64G", f"{shards and ''}主从分离"),
        ("MySQL", mysql_hosts, "16C64G SSD", f"{shards} 主 + {shards} 从"),
        ("其他服务", OTHER_MACHINES, "—", "Auth / Social / Task / Mail / Kafka / 运维"),
    ]
    print(L("组件", 16) + R("台数", 8) + R("规格", 16) + "   说明")
    print("-" * 100)
    total = 0
    for name, n, spec, note in rows:
        print(L(name, 16) + R(n, 8) + R(spec, 16) + "   " + note)
        total += n
    print("-" * 100)
    print(L("合计", 16) + R(total, 8))
    return total


# ============================================================================
# 检查 9 · bench 时间档的压测加速比
# ============================================================================

def check_bench_profile():
    rule("检查 9 · bench 时间档的压测加速比（策划 3.2 预留的旋钮）")
    radish_real_sec = RADISH_HOURS * 3600 * TIME_SCALE_BENCH
    day_real_sec = 24 * 3600 * TIME_SCALE_BENCH
    logic_day_real_sec = max(day_real_sec, LOGIC_DAY_MIN_SECONDS)

    kv("TIME_SCALE", f"1/{int(1 / TIME_SCALE_BENCH)}")
    kv("白萝卜一轮", f"{radish_real_sec:.0f} 真实秒", f"{RADISH_HOURS} 缩放小时")
    kv("一个作物日（24 缩放小时）", f"{day_real_sec:.0f} 真实秒")
    kv("一个逻辑日", f"{logic_day_real_sec:.0f} 真实秒", "策划 3.4 有 5 分钟下限")
    kv("5 分钟压测 = 作物日", f"{300 / day_real_sec:.1f} 天", "同时恰好覆盖 1 个逻辑日边界")

    print("\n这意味着一轮 5 分钟的压测可以完整覆盖：")
    print("  · 十余次完整种植循环（锄地 -> 播种 -> 照料 -> 成熟 -> 收获 -> 清理）")
    print("  · 一次逻辑日重置（每日上限归零、日常任务重抽）")
    print("  · 多轮偷菜窗口的开闭")
    print("没有这个旋钮，压出同样的行为覆盖需要真实运行 12 天。")

    assert_that(logic_day_real_sec <= 600,
                "bench 档下逻辑日超过 10 分钟，单轮压测无法覆盖每日重置")
    return radish_real_sec


# ============================================================================
# 检查 10 · 压测通过线反推
# ============================================================================

def check_benchmark_target(peak_qps_design, fs_machines, radish_real_sec):
    rule("检查 10 · 压测通过线反推")
    pass_line = peak_qps_design / (fs_machines * EXTRAPOLATION_DISCOUNT)

    print("外推模型：集群总吞吐 = 单机实测拐点 x 机器数 x 折扣系数")
    print("折扣系数 %.2f 用于扣除内部 RPC、跨机广播、路由查表、尾延迟叠加等"
          "单机压测测不到的开销。\n" % EXTRAPOLATION_DISCOUNT)
    kv("峰值 QPS 设计值", wan(peak_qps_design))
    kv("FarmServer 台数", f"{fs_machines} 台")
    kv("折扣系数", f"{EXTRAPOLATION_DISCOUNT}")
    kv("单机压测通过线", f"{pass_line:,.0f} QPS", "P99 未爆炸前的稳定吞吐")
    kv("配置区假设值", f"{QPS_PER_FARMSERVER:,} QPS")
    margin = QPS_PER_FARMSERVER / pass_line
    kv("假设值 / 通过线", f"{margin:.2f}x", "大于 1 表示容量模型自洽且有余量")

    assert_that(margin >= 1.0,
                f"单机 QPS 假设值低于压测通过线（{margin:.2f}x），"
                f"容量模型自相矛盾：要么加机器，要么提高单机性能目标")

    # 压测客户端规模
    ops_per_cycle = FIXED_OPS_PER_CYCLE + WATERINGS_PER_SEASON + HAZARDS_PER_SEASON
    ops_per_sec_per_robot = (ops_per_cycle * INITIAL_PLOTS / radish_real_sec
                             * SOCIAL_OPS_MULTIPLIER)
    robots = math.ceil(pass_line / ops_per_sec_per_robot)

    print()
    kv("单地块单轮操作数", f"{ops_per_cycle:.1f} 次",
       "锄地+播种+收获+清理+3 浇水+2.2 除草除虫")
    kv("单机器人操作频率", f"{ops_per_sec_per_robot:.1f} 次/秒",
       f"{INITIAL_PLOTS} 块地并行，含 {(SOCIAL_OPS_MULTIPLIER - 1) * 100:.0f}% 社交操作")
    kv("打到通过线所需机器人", f"{robots:,} 个", "即压测客户端需维持的连接数")
    kv("压测客户端机器", f"{math.ceil(robots / 50_000)} 台", "按单台 5 万连接计")

    assert_that(robots <= CONN_PER_GATEWAY,
                f"所需机器人数 {robots} 超过单网关连接上限，压测拓扑需要多网关")
    return pass_line, robots


# ============================================================================
# 主流程
# ============================================================================

def main():
    print("=" * 100)
    print("经典农场 · 容量估算与压测通过线推导")
    print(f"DAU = {DAU:,}")
    print("=" * 100)

    daily_req = DAU * SESSIONS_PER_DAY * OPS_PER_SESSION

    _, pcu, pcu_design = check_concurrency()
    peak_qps, peak_qps_design, _, peak_write_design, out_total = check_qps(pcu, pcu_design)
    mem_per_user, db_per_user = check_per_user_bytes()
    _, shards = check_storage(db_per_user, peak_write_design, daily_req)
    gw, _ = check_gateway(pcu_design, out_total)
    fs = check_farmserver(pcu_design, peak_qps_design, mem_per_user)
    redis_hosts = check_redis()
    check_machines(gw, fs, redis_hosts, shards)
    radish_real_sec = check_bench_profile()
    check_benchmark_target(peak_qps_design, fs, radish_real_sec)

    rule("结果")
    if FAILURES:
        print(f"{len(FAILURES)} 项断言失败：")
        for f in FAILURES:
            print(f"  · {f}")
        return 1
    print("全部断言通过。")
    print("\n提醒：本脚本的输入绝大多数是「假设」而非「事实」。")
    print("压测完成后必须用实测值替换配置区 C 的单机能力假设，并重跑本脚本。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
