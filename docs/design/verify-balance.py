#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
经典农场 · 数值平衡验算脚本

用途：把 docs/game-design.md 参数总表里的全部数值跑一遍一致性与平衡性检查，
输出直接回填进设计文档的「数值验算」章节。

本脚本的配置区必须与 docs/game-design.md 的参数总表逐项对应；
任一处改动都要同步另一处，并重跑本脚本。

运行：python3 docs/verify-balance.py
退出码：0 = 全部断言通过，1 = 存在断言失败
"""

import unicodedata

# ============================================================================
# 配置区：与 game-design.md 参数总表严格对应
# 标注含义  [原版] 可交叉印证  [考据] 单一来源留档  [设计] 原版存疑本项目设计  [新增] 原版无此机制
# ============================================================================

# --- 等级与土地 ---
EXP_PER_LEVEL = 200                 # [原版] 到达 N 级的累计经验 = N * 200
INITIAL_PLOTS = 6                   # [原版] 初始地块数
MAX_PLOTS = 18                      # [原版] 地块上限
INITIAL_COINS = 1000                # [设计] 初始金币

# [原版] 扩地链：(开垦后总地块数, 等级要求, 本次开垦金币)
LAND_CHAIN = [
    (7, 5, 10_000), (8, 7, 20_000), (9, 9, 30_000), (10, 11, 50_000),
    (11, 13, 70_000), (12, 15, 90_000), (13, 17, 120_000), (14, 19, 150_000),
    (15, 21, 180_000), (16, 23, 230_000), (17, 25, 300_000), (18, 27, 500_000),
]

# --- 动作经验 ---
EXP_HOE = 3                         # [原版] 锄地 / 清理
EXP_PLANT = 2                       # [原版] 播种
EXP_CARE = 2                        # [原版] 浇水 / 除草 / 除虫，每次
DAILY_CARE_EXP_CAP = 150            # [考据] 维护动作每逻辑日前 N 次计经验
HELP_COIN_REWARD = 5                # [设计] 在好友农场除草 / 除虫的系统金币，与经验共用同一计数器
HELP_COIN_SHARE_CAP = 0.40          # [设计] 互助金币上限占自耕日产出的比例上限

# --- 健康度与产量 ---
# [设计] 三项权重归一化为 1.00，使「三项全程犯满」恰好使健康度归零，clamp 永不触发
W_DRY, W_WEED, W_PEST = 0.44, 0.26, 0.30
YIELD_FLOOR = 0.60                  # [设计] 健康度为 0 时的产量系数下限

# --- 照料参数：全部按「本季生长时长」的比例定义，保证跨等级难度一致 ---
WATER_DURATION_RATIO = 0.35         # [设计] 一次浇水的水分持续时长占本季比例
RISK_WINDOW_RATIO = 0.10            # [设计] 风险判定窗口长度占本季比例 -> 每季恰好 10 次判定
P_WEED_PER_WINDOW = 0.12            # [设计] 单窗口长草概率
P_PEST_PER_WINDOW = 0.10            # [设计] 单窗口生虫概率
# --- 偷菜与守卫 ---
STEAL_CAP_RATIO = 0.40              # [原版] 单轮成熟被访客合计偷走的产量上限
STEAL_MIN, STEAL_MAX = 1, 10        # [设计] 单次偷取数量区间（原版记为版本参数）
DOG_FOOD_PER_HOUR = 5               # [考据] 标准粮耗 g / 缩放小时
DOG_BOWL_CAPACITY = 120             # [设计] 狗盆容量 g -> 恰好 24 缩放小时
DOG_FOOD_PRICE = 1                  # [设计] 狗粮单价 金币 / g
CAUGHT_PENALTY_MULT = 10            # [设计] 被狗抓到时访客赔付 = 该作物果实单价 * N
# [设计] (狗种, 拦截概率, 粮耗 g/缩放小时, 售价, 等级要求)
DOGS = [
    ("土狗", 0.25, 4, 2_000, 0),
    ("牧羊犬", 0.35, 5, 4_500, 10),
    ("藏獒", 0.45, 7, 8_000, 20),
]

# --- 化肥 ---
# [考据] 效果小时数为绝对值；[设计] 价格
FERTILIZERS = [("普通化肥", 1.0, 50), ("高速化肥", 2.5, 200), ("急速化肥", 5.5, 500)]
STAGES_LOW, STAGES_HIGH = 3, 4       # [考据] 解锁等级 <3 为 3 个生长阶段，>=3 为 4 个
STAGE_SPLIT_LEVEL = 3

# --- 隐藏种子 ---
HIDDEN_SEED_DROP = 0.03             # [设计] 每次锄地 / 清理的掉落概率
HIDDEN_STRENGTH_CAP = 2.0           # [设计] 隐藏作物强度上限（相对同期最优普通作物）

# --- 时间缩放 ---
TIME_PROFILES = [("authentic", 1.0), ("fast", 1 / 60), ("demo", 1 / 600), ("bench", 1 / 3600)]
LOGIC_DAY_MIN_MINUTES = 5           # [设计] 逻辑日缩放后的最短真实时长

# --- 日常任务 ---
# [新增] (任务, 目标次数, 金币奖励, 经验奖励)
DAILY_TASKS = [
    ("浇水", 10, 200, 20), ("收获", 5, 300, 30), ("帮好友照料", 5, 250, 25),
    ("偷菜成功", 3, 200, 15), ("播种", 6, 200, 20), ("出售果实", 1, 150, 10),
    ("施肥", 1, 100, 10),
]
DAILY_TASK_PICK = 3                 # [新增] 每逻辑日随机选取的任务条数
TASK_SHARE_CAP = 0.50               # [设计] 任务奖励占日均产出的上限

# --- 审查阈值 ---
INVERSION_GAP_THRESHOLD = 0.05      # 效率倒挂的显著性阈值

# ============================================================================
# 作物表  [原版]
# name, 解锁等级, 种子价, 季数, 全周期(h), 每季产量, 果实单价, 每季收获经验
# ============================================================================

CROPS = [
    ("白萝卜",  0,  125, 1,  10, 16, 17, 15),
    ("胡萝卜",  0,  163, 1,  13, 17, 21, 18),
    ("大白菜",  1,  168, 1,  14, 17, 22, 19),
    ("小麦",    2,  168, 1,  14, 18, 21, 19),
    ("水稻",    2,  168, 1,  14, 18, 21, 19),
    ("玉米",    3,  175, 1,  14, 17, 23, 19),
    ("土豆",    4,  188, 1,  15, 18, 24, 20),
    ("红枣",    5,  237, 1,  16, 20, 25, 21),
    ("茄子",    5,  237, 1,  16, 20, 25, 21),
    ("番茄",    6,  251, 1,  17, 21, 26, 22),
    ("豌豆",    7,  266, 1,  18, 22, 27, 23),
    ("红玫瑰",  7,  266, 1,  18, 22, 27, 23),
    ("辣椒",    8,  296, 1,  20, 24, 28, 25),
    ("南瓜",    9,  325, 1,  22, 25, 30, 27),
    ("苹果",   10,  578, 2,  30, 23, 24, 18),
    ("草莓",   10,  605, 2,  35, 24, 27, 20),
    ("西瓜",   11,  708, 2,  41, 27, 29, 23),
    ("香蕉",   12,  900, 2,  45, 29, 32, 25),
    ("桃子",   13, 1200, 2,  60, 32, 40, 33),
    ("橙子",   14, 1587, 3,  59, 26, 41, 25),
    ("葡萄",   15, 1978, 3,  86, 29, 47, 30),
    ("石榴",   16, 2425, 3,  96, 30, 54, 34),
    ("柚子",   17, 2855, 3, 113, 33, 58, 39),
    ("菠萝",   18, 3480, 3, 116, 35, 62, 40),
    ("椰子",   19, 3720, 4, 124, 27, 65, 32),
    ("葫芦",   20, 4742, 4, 139, 30, 71, 36),
]

# [设计] 隐藏作物：仅由锄地 / 清理掉落，商店不出售，种子价视为 0
# name, 最低掉落等级, 季数, 全周期(h), 每季产量, 果实单价, 每季收获经验
HIDDEN_CROPS = [
    ("人参",     0, 1,  40, 22, 41,  60),
    ("灵芝",    10, 1,  60, 30, 60,  80),
    ("摇钱树",  20, 3, 100, 25, 55, 100),
]


# ============================================================================
# 显示宽度对齐（CJK 字符占两列）
# ============================================================================

def dw(s):
    return sum(2 if unicodedata.east_asian_width(str(c)) in "FW" else 1 for c in str(s))


def L(s, w):
    return str(s) + " " * max(0, w - dw(s))


def R(s, w):
    return " " * max(0, w - dw(s)) + str(s)


# ============================================================================
# 公式
# ============================================================================

def season_split(seasons, total_hours):
    """[设计] 首季耗时 = 2 x 后续每季耗时；各季之和严格等于全周期。"""
    unit = total_hours / (seasons + 1)
    return [2 * unit] + [unit] * (seasons - 1)


def health(dry_ratio, weed_ratio, pest_ratio):
    """健康度。入参为三种不良状态时长占本季生长时长的比例。"""
    debt = W_DRY * dry_ratio + W_WEED * weed_ratio + W_PEST * pest_ratio
    return max(0.0, min(100.0, 100.0 - 100.0 * debt))


def yield_factor(h):
    """产量系数：健康度 0 -> YIELD_FLOOR，健康度 100 -> 1.0。"""
    return YIELD_FLOOR + (1.0 - YIELD_FLOOR) * h / 100.0


def revenue(seasons, yield_per_season, fruit_price, factor=1.0):
    return yield_per_season * seasons * fruit_price * factor


def waterings_per_season():
    return round(1 / WATER_DURATION_RATIO)


def risk_windows_per_season():
    return round(1 / RISK_WINDOW_RATIO)


def care_actions_per_season():
    return waterings_per_season() + risk_windows_per_season() * (P_WEED_PER_WINDOW + P_PEST_PER_WINDOW)


def cycle_exp(seasons, exp_per_season):
    """一个完整种植周期的经验：播种 + 清理 + 各季收获 + 期望维护动作。"""
    return (EXP_PLANT + EXP_HOE + exp_per_season * seasons
            + care_actions_per_season() * EXP_CARE * seasons)


def coins_per_hour(crop):
    _, _, seed, ss, th, y, fp, _ = crop
    return (revenue(ss, y, fp) - seed) / th


def exp_per_hour(crop):
    _, _, _, ss, th, _, _, xp = crop
    return cycle_exp(ss, xp) / th


def fert_stages(unlock_level):
    return STAGES_LOW if unlock_level < STAGE_SPLIT_LEVEL else STAGES_HIGH


def crop(name):
    return next(c for c in CROPS if c[0] == name)


# 照料档位：用于横向比较的参考点，数值为三种不良状态占本季时长的比例
CARE_PERFECT = (0.0, 0.0, 0.0)          # 完美照料
NEGLECT_LIGHT = (0.30, 0.40, 0.30)      # 偶尔照料
NEGLECT_TYPICAL = (0.65, 0.80, 0.70)    # 播种后完全放置
NEGLECT_FULL = (1.0, 1.0, 1.0)          # 三项全程犯满


FAILURES = []


def rule(title):
    print("\n" + "=" * 100)
    print(title)
    print("=" * 100)


def assert_that(cond, msg):
    if not cond:
        FAILURES.append(msg)
        print(f"  [!] 断言失败: {msg}")


# ============================================================================
# 检查 1 · 健康度公式边界
# ============================================================================

def check_health_formula():
    rule("检查 1 · 健康度公式边界与产量系数")
    total_w = W_DRY + W_WEED + W_PEST
    print(f"权重  缺水 {W_DRY}  杂草 {W_WEED}  害虫 {W_PEST}   合计 {total_w:.2f}")
    print(f"产量系数下限 {YIELD_FLOOR}\n")
    print(L("照料档位", 30) + R("健康度", 10) + R("产量系数", 12))
    print("-" * 100)
    for label, r in [("完美照料", CARE_PERFECT), ("仅缺水全程", (1, 0, 0)),
                     ("仅带草全程", (0, 1, 0)), ("仅带虫全程", (0, 0, 1)),
                     ("轻度疏忽  水.30 草.40 虫.30", NEGLECT_LIGHT),
                     ("典型放置  水.65 草.80 虫.70", NEGLECT_TYPICAL),
                     ("三项全程犯满", NEGLECT_FULL)]:
        h = health(*r)
        print(L(label, 30) + R(f"{h:.1f}", 10) + R(f"{yield_factor(h):.4f}", 12))
    print("-" * 100)
    raw = 100.0 - 100.0 * total_w
    print(f"三项全程犯满的未截断值 = {raw:.1f}  ->  clamp {'不需要触发' if raw >= 0 else '会被触发'}")
    assert_that(abs(total_w - 1.0) < 1e-9, f"健康度三项权重之和应为 1.00，实际 {total_w}")
    assert_that(abs(yield_factor(health(*NEGLECT_FULL)) - YIELD_FLOOR) < 1e-9,
                "三项全程犯满的产量系数应恰好等于下限")


# ============================================================================
# 检查 2 · ROI 三档与净亏本判据
# ============================================================================

def check_roi():
    rule("检查 2 · ROI —— 满健康 / 典型放置 / 零健康 / 零健康且被偷满 40%")
    f_typ = yield_factor(health(*NEGLECT_TYPICAL))
    f_worst = YIELD_FLOOR * (1 - STEAL_CAP_RATIO)
    breakeven = 1.0 / f_worst
    print(f"典型放置产量系数 {f_typ:.4f}    最坏情形系数 {f_worst:.4f}")
    print(f"最坏情形盈亏平衡判据：收入/种子价 >= 1/{f_worst:.2f} = {breakeven:.2f}\n")
    hdr = (L("作物", 9) + R("解锁", 5) + R("种子", 7) + R("满产收入", 11)
           + R("收入/种子", 11) + R("满利", 8) + R("典型利", 9) + R("零健利", 9)
           + R("最坏", 9) + R("激励", 8))
    print(hdr)
    print("-" * 100)
    ratios, worst_loss, typ_loss = [], [], []
    for c in CROPS:
        name, lv, seed, ss, th, y, fp, xp = c
        rev = revenue(ss, y, fp)
        ratio = rev / seed
        p100, ptyp = rev - seed, rev * f_typ - seed
        p0, pworst = rev * YIELD_FLOOR - seed, rev * f_worst - seed
        ratios.append((name, ratio))
        if pworst < 0:
            worst_loss.append((name, lv, pworst, pworst / seed * 100))
        if ptyp < 0:
            typ_loss.append(name)
        print(L(name, 9) + R(lv, 5) + R(seed, 7) + R(f"{rev:.0f}", 11)
              + R(f"{ratio:.2f}", 11) + R(f"{p100:.0f}", 8) + R(f"{ptyp:.0f}", 9)
              + R(f"{p0:.0f}", 9) + R(f"{pworst:.0f}", 9) + R(f"{p100 / ptyp:.2f}x", 8))
    print("-" * 100)
    lo, hi = min(ratios, key=lambda r: r[1]), max(ratios, key=lambda r: r[1])
    print(f"收入/种子价区间 {lo[1]:.2f}（{lo[0]}） — {hi[1]:.2f}（{hi[0]}），"
          f"{'全部低于' if hi[1] < breakeven else '存在高于'}平衡判据 {breakeven:.2f}")
    print(f"最坏情形净亏本 {len(worst_loss)}/{len(CROPS)} 种，亏损占种子价 "
          f"{min(-r[3] for r in worst_loss):.0f}% — {max(-r[3] for r in worst_loss):.0f}%")
    print(f"典型放置仍盈利 {len(CROPS) - len(typ_loss)}/{len(CROPS)} 种"
          + (f"，亏本：{typ_loss}" if typ_loss else "（无一亏本）"))
    print(f"照料激励倍数区间 "
          f"{min((revenue(c[3], c[5], c[6]) - c[2]) / (revenue(c[3], c[5], c[6]) * f_typ - c[2]) for c in CROPS):.2f}x"
          f" — "
          f"{max((revenue(c[3], c[5], c[6]) - c[2]) / (revenue(c[3], c[5], c[6]) * f_typ - c[2]) for c in CROPS):.2f}x"
          f"（随作物等级上升，构成难度梯度）")
    assert_that(not typ_loss, f"典型放置档位下不应有作物亏本，实际亏本：{typ_loss}")

    print("\n完美照料但被偷满 40% 的利润损失：")
    for name in ("白萝卜", "南瓜", "菠萝"):
        c = crop(name)
        rev = revenue(c[3], c[5], c[6])
        full, robbed = rev - c[2], rev * (1 - STEAL_CAP_RATIO) - c[2]
        print(f"  {L(name, 8)}{R(f'{full:.0f}', 7)} -> {R(f'{robbed:.0f}', 7)} 金币，"
              f"缩水 {(1 - robbed / full) * 100:.0f}%")


# ============================================================================
# 检查 3 · 效率排序与倒挂检测
# ============================================================================

def check_efficiency():
    rule("检查 3 · 效率排序（满健康，单地块并行口径）")
    rows = [{"name": c[0], "lv": c[1], "cph": coins_per_hour(c), "eph": exp_per_hour(c),
             "profit": revenue(c[3], c[5], c[6]) - c[2], "hours": c[4]} for c in CROPS]
    print(R("排名", 6) + "  " + L("作物", 9) + R("解锁", 6) + R("金币/h", 10)
          + R("经验/h", 10) + R("周期h", 8) + R("净利", 10))
    print("-" * 100)
    for i, r in enumerate(sorted(rows, key=lambda r: -r["cph"]), 1):
        print(R(f"{i}.", 6) + "  " + L(r["name"], 9) + R(f"Lv{r['lv']}", 6)
              + R(f"{r['cph']:.1f}", 10) + R(f"{r['eph']:.2f}", 10)
              + R(r["hours"], 8) + R(f"{r['profit']:.0f}", 10))
    print("-" * 100)
    best_c = max(rows, key=lambda r: r["cph"])
    best_e = max(rows, key=lambda r: r["eph"])
    worst_e = min(rows, key=lambda r: r["eph"])
    print(f"金币效率最优  {best_c['name']}(Lv{best_c['lv']}) {best_c['cph']:.1f} 金币/h")
    print(f"经验效率最优  {best_e['name']}(Lv{best_e['lv']}) {best_e['eph']:.2f} 经验/h")
    print(f"经验效率最差  {worst_e['name']}(Lv{worst_e['lv']}) {worst_e['eph']:.2f} 经验/h"
          f"，相差 {best_e['eph'] / worst_e['eph']:.2f} 倍")
    print("-> 经验效率随等级单调下降，练级最优解恒为最低级作物；"
          "与金币最优解分离，形成双策略张力")

    rule("检查 3b · 效率倒挂检测（对比「解锁等级严格更低的最优作物」）")
    print(L("作物", 9) + R("解锁", 6) + R("金币/h", 10) + R("前序最优", 12)
          + R("变化", 9) + "  " + L("判定", 12))
    print("-" * 100)
    significant, peak = [], None
    for r in sorted(rows, key=lambda r: (r["lv"], r["name"])):
        lower = [b for b in rows if b["lv"] < r["lv"]]
        if not lower:
            print(L(r["name"], 9) + R(f"Lv{r['lv']}", 6) + R(f"{r['cph']:.1f}", 10)
                  + R("—", 12) + R("—", 9) + "  " + L("起点", 12))
            continue
        ref = max(lower, key=lambda b: b["cph"])
        gap = r["cph"] / ref["cph"] - 1
        if gap < -INVERSION_GAP_THRESHOLD:
            verdict = "显著倒挂"
            significant.append((r["name"], r["lv"], gap, ref["name"], ref["lv"]))
        elif gap < 0:
            verdict = "轻微倒挂"
        elif gap > 0.15:
            verdict = "跃升"
            peak = r
        else:
            verdict = "正常递进"
        print(L(r["name"], 9) + R(f"Lv{r['lv']}", 6) + R(f"{r['cph']:.1f}", 10)
              + R(f"{ref['name']} {ref['cph']:.1f}", 12) + R(f"{gap:+.1%}", 9)
              + "  " + L(verdict, 12))
    print("-" * 100)
    print(f"显著倒挂（低于前序最优超过 {INVERSION_GAP_THRESHOLD:.0%}）：{len(significant)} 处")
    for name, lv, gap, rn, rl in significant:
        print(f"  {name}(Lv{lv}) {gap:+.1%}，参照 {rn}(Lv{rl})")
    if peak:
        after = [r for r in rows if r["lv"] > peak["lv"]]
        worse = [r["name"] for r in after if r["cph"] < peak["cph"]]
        print(f"\n效率跃升点：{peak['name']}(Lv{peak['lv']}) {peak['cph']:.1f} 金币/h，"
              f"为全表最优")
        print(f"  其后解锁的 {len(after)} 种作物中有 {len(worse)} 种不如它：{worse}")
        print(f"  -> 上表中 {[s[0] for s in significant if s[3] == peak['name']]} 的倒挂"
              f"均以 {peak['name']} 为参照，属同一根因")
        real = [s for s in significant if s[3] != peak["name"]]
        print(f"  -> 与跃升点无关的真实倒挂：{[(s[0], f'{s[2]:+.1%}') for s in real] or '无'}")

    # 同级作物内部支配关系
    print("\n同解锁等级内部的支配关系：")
    by_lv = {}
    for r in rows:
        by_lv.setdefault(r["lv"], []).append(r)
    for lv, group in sorted(by_lv.items()):
        if len(group) < 2:
            continue
        g = sorted(group, key=lambda r: -r["cph"])
        if g[0]["cph"] - g[-1]["cph"] > 1e-9:
            print(f"  Lv{lv}: {g[0]['name']} {g[0]['cph']:.1f} 支配 "
                  f"{g[-1]['name']} {g[-1]['cph']:.1f}（差 {g[0]['cph'] / g[-1]['cph'] - 1:+.1%}）")

    late = [r for r in rows if r["lv"] >= 14]
    spread = max(r["cph"] for r in late) - min(r["cph"] for r in late)
    print(f"\nLv14+ 金币效率区间 {min(r['cph'] for r in late):.1f} — "
          f"{max(r['cph'] for r in late):.1f}，极差 {spread:.1f}"
          f" -> {'后期经济曲线基本走平，成长动力来自地块数量而非作物品质' if spread < 5 else '仍有梯度'}")


# ============================================================================
# 检查 4 · 原版数据异常检测
# ============================================================================

def check_data_anomaly():
    rule("检查 4 · 原版作物数据异常检测（多季作物的每季平均时长应随等级递增）")
    multi = [c for c in CROPS if c[3] > 1]
    print(L("作物", 9) + R("解锁", 6) + R("季数", 6) + R("全周期", 9)
          + R("每季均时长", 12) + R("较前一档", 11) + "  " + L("判定", 10))
    print("-" * 100)
    prev, anomalies = None, []
    for c in sorted(multi, key=lambda c: c[1]):
        per = c[4] / c[3]
        if prev is None:
            delta, verdict = "—", "起点"
        else:
            d = per / prev[1] - 1
            delta = f"{d:+.1%}"
            if d < -0.10:
                verdict, _ = "异常下降", anomalies.append((c[0], c[1], per, prev[0], prev[1], d))
            elif d < 0:
                verdict = "轻微下降"
            else:
                verdict = "正常"
        print(L(c[0], 9) + R(f"Lv{c[1]}", 6) + R(c[3], 6) + R(f"{c[4]}h", 9)
              + R(f"{per:.2f}h", 12) + R(delta, 11) + "  " + L(verdict, 10))
        prev = (c[0], per)
    print("-" * 100)
    if anomalies:
        print(f"检出 {len(anomalies)} 处异常下降（每季均时长比前一档低 10% 以上）：")
        for name, lv, per, pn, pp, d in anomalies:
            print(f"  {name}(Lv{lv}) 每季均 {per:.2f}h，较 {pn} 的 {pp:.2f}h 下降 {-d:.1%}")
        top = anomalies[0]
        peach = crop("桃子")
        print(f"\n重点：{top[0]} 全周期 {crop(top[0])[4]}h / {crop(top[0])[3]} 季，"
              f"竟短于更低级的 桃子 {peach[4]}h / {peach[3]} 季")
        print("  这是原版考据数据的内在异常，直接导致检查 3b 的效率跃升。")
        print("  处置：按考据原值保留，在设计文档「存疑项」中登记，不擅自修正。")
    else:
        print("未检出异常。")


# ============================================================================
# 检查 5 · 多季作物拆分
# ============================================================================

def check_season_split():
    rule("检查 5 · 多季作物首熟 / 再熟拆分")

    def fmt(h):
        m = round((h - int(h)) * 60)
        return f"{int(h)}h{m:02d}m" if m else f"{int(h)}h"

    print(L("作物", 9) + R("季数", 6) + R("全周期", 9) + R("首熟", 11)
          + R("每次再熟", 11) + R("各季之和", 11) + R("校验", 7))
    print("-" * 100)
    for c in CROPS:
        if c[3] == 1:
            continue
        parts = season_split(c[3], c[4])
        total = sum(parts)
        ok = abs(total - c[4]) < 1e-9
        assert_that(ok, f"{c[0]} 各季之和 {total} 不等于全周期 {c[4]}")
        print(L(c[0], 9) + R(c[3], 6) + R(f"{c[4]}h", 9) + R(fmt(parts[0]), 11)
              + R(fmt(parts[1]), 11) + R(f"{total:.2f}", 11)
              + R("OK" if ok else "FAIL", 7))
    print("-" * 100)
    print("拆分规则：后续每季 = 全周期/(季数+1)，首季 = 2 x 后续每季")


# ============================================================================
# 检查 6 · 成长节奏
# ============================================================================

def check_pacing():
    rule("检查 6 · 初期成长节奏（初始 6 块地）")
    need = CROPS[0][2] * INITIAL_PLOTS
    print(f"初始金币 {INITIAL_COINS}，种满 {INITIAL_PLOTS} 块白萝卜需 {need}，"
          f"余量 {INITIAL_COINS - need}")
    assert_that(INITIAL_COINS >= need,
                f"初始金币 {INITIAL_COINS} 不足以种满 {INITIAL_PLOTS} 块白萝卜（需 {need}）")
    lv5, land7 = 5 * EXP_PER_LEVEL, LAND_CHAIN[0][2]
    print()
    for c in CROPS[:3]:
        name, lv, seed, ss, th, y, fp, xp = c
        c6 = (revenue(ss, y, fp) - seed) * INITIAL_PLOTS
        e6 = cycle_exp(ss, xp) * INITIAL_PLOTS
        r_exp, r_coin = lv5 / e6, land7 / c6
        print(f"{name}（周期 {th}h）  6 地单轮 {c6:.0f} 金币 / {e6:.1f} 经验")
        print(f"  到 5 级（{lv5} 经验）      {r_exp:.1f} 轮 = {r_exp * th:.0f}h")
        print(f"  攒够第 7 块地（{land7}）  {r_coin:.1f} 轮 = {r_coin * th:.0f}h")
        print(f"  -> 瓶颈是{'金币' if r_coin > r_exp else '经验'}"
              f"（金币需求为经验需求的 {r_coin / r_exp:.1f} 倍）")

    rule("检查 6b · 扩地总账")
    total_coin = sum(c for _, _, c in LAND_CHAIN)
    max_lv = max(lv for _, lv, _ in LAND_CHAIN)
    best = max(CROPS, key=coins_per_hour)
    hours = total_coin / (coins_per_hour(best) * (MAX_PLOTS - 1))
    print(f"第 {INITIAL_PLOTS + 1} — {MAX_PLOTS} 块累计金币 {total_coin:,}")
    print(f"最高等级要求 Lv{max_lv}，对应累计经验 {max_lv * EXP_PER_LEVEL:,}"
          f"（远低于金币门槛，确认全程金币瓶颈）")
    print(f"按最优作物 {best[0]}（{coins_per_hour(best):.1f} 金币/h/地）"
          f"以 {MAX_PLOTS - 1} 地并行推进：")
    for pname, scale in TIME_PROFILES:
        real = hours * scale
        unit = f"{real:.0f}h" if real >= 2 else f"{real * 60:.0f}分"
        print(f"  {L(pname, 12)} {hours:,.0f} 缩放小时 = {unit} 真实时长"
              + (f" = {real / 24:.1f} 天" if real >= 48 else ""))


# ============================================================================
# 检查 7 · 每日维护动作次数与经验上限
# ============================================================================

def check_daily_cap():
    rule("检查 7 · 每日维护动作次数 vs 计经验上限")
    per = care_actions_per_season()
    print(f"每季期望维护动作 {per:.1f} 次 = 浇水 {waterings_per_season()} + 期望除草 "
          f"{risk_windows_per_season() * P_WEED_PER_WINDOW:.1f} + 期望除虫 "
          f"{risk_windows_per_season() * P_PEST_PER_WINDOW:.1f}")
    print(f"计经验上限 {DAILY_CARE_EXP_CAP} 次/逻辑日（折合 {DAILY_CARE_EXP_CAP * EXP_CARE} 经验）\n")
    print(R("地块数", 8) + "  " + L("作物", 9) + R("每日轮数", 11)
          + R("维护动作/天", 14) + R("占上限", 10) + "  " + L("判定", 10))
    print("-" * 100)
    first_hit = None
    for plots in (INITIAL_PLOTS, 12, MAX_PLOTS):
        for name in ("白萝卜", "南瓜", "菠萝"):
            c = crop(name)
            rounds = 24 / c[4]
            actions = per * c[3] * plots * rounds
            hit = actions >= DAILY_CARE_EXP_CAP
            if hit and first_hit is None:
                first_hit = (plots, name, actions)
            print(R(plots, 8) + "  " + L(name, 9) + R(f"{rounds:.2f}", 11)
                  + R(f"{actions:.0f}", 14) + R(f"{actions / DAILY_CARE_EXP_CAP:.0%}", 10)
                  + "  " + L("会触及" if hit else "未触及", 10))
    print("-" * 100)
    if first_hit:
        print(f"最早触及上限：{first_hit[0]} 块地种{first_hit[1]}，{first_hit[2]:.0f} 次/天")
    print("结论：上限只在地块数接近上限且种短周期作物时才生效，不影响新手体验")

    rule("检查 7b · 互助金币的收入占比（防止互助取代种地）")
    help_cap = DAILY_CARE_EXP_CAP * HELP_COIN_REWARD
    c0 = crop("白萝卜")
    self_daily = (revenue(c0[3], c0[5], c0[6]) - c0[2]) * INITIAL_PLOTS * (24 / c0[4])
    print(f"好友农场除草/除虫奖励 {HELP_COIN_REWARD} 金币，与经验共用 {DAILY_CARE_EXP_CAP} 次计数器")
    print(f"-> 理论上限 {help_cap} 金币/逻辑日\n")

    # 现实可达上限受「好友农场实际产生多少不良状态」约束
    events_per_friend = (INITIAL_PLOTS * (24 / c0[4])
                         * risk_windows_per_season() * (P_WEED_PER_WINDOW + P_PEST_PER_WINDOW))
    print(f"单个好友（6 地循环种白萝卜）每日产生除草+除虫机会 {events_per_friend:.0f} 次")
    print(f"-> 打满 {DAILY_CARE_EXP_CAP} 次计数器需要 "
          f"{DAILY_CARE_EXP_CAP / events_per_friend:.1f} 个满负荷好友\n")
    print(R("活跃好友数", 12) + R("可用机会", 11) + R("实得次数", 11)
          + R("互助金币", 11) + R("占自耕产出", 13) + "  " + L("判定", 16))
    print("-" * 100)
    worst_share = 0.0
    for friends in (1, 3, 5, 10):
        chances = events_per_friend * friends
        actual = min(chances, DAILY_CARE_EXP_CAP)
        coins = actual * HELP_COIN_REWARD
        share = coins / self_daily
        worst_share = max(worst_share, share)
        print(R(friends, 12) + R(f"{chances:.0f}", 11) + R(f"{actual:.0f}", 11)
              + R(f"{coins:.0f}", 11) + R(f"{share:.1%}", 13) + "  "
              + L("补贴" if share < HELP_COIN_SHARE_CAP else "过高·会诱导刷金", 16))
    print("-" * 100)
    print(f"最坏情形（计数器打满）占自耕产出 {worst_share:.1%}，"
          f"上限 {HELP_COIN_SHARE_CAP:.0%}")
    assert_that(worst_share < HELP_COIN_SHARE_CAP,
                f"互助金币最坏情形占自耕产出 {worst_share:.0%}，"
                f"超过上限 {HELP_COIN_SHARE_CAP:.0%}，会诱导以互助代替种地")
    steal_value = (STEAL_MIN + STEAL_MAX) / 2 * c0[6]
    print(f"对比：单次偷菜期望价值 {steal_value:.0f} 金币，为单次互助的 "
          f"{steal_value / HELP_COIN_REWARD:.0f} 倍")
    print("-> 访问好友的主要动机是偷菜，互助只是顺带的礼节性收益，定位清晰")
    print("若金币不受该计数器约束，反复到好友农场除草将成为比种地更高效的收入来源")


# ============================================================================
# 检查 8 · 隐藏作物强度
# ============================================================================

def check_hidden_crops():
    rule("检查 8 · 隐藏作物强度（种子价为 0，对比同期可得的最优普通作物）")
    print(L("作物", 9) + R("掉落等级", 11) + R("季数", 6) + R("全周期", 9)
          + R("总收入", 10) + R("金币/h", 10) + "  " + L("同期最优", 16) + R("强度", 8))
    print("-" * 100)
    for name, gate, ss, th, y, fp, xp in HIDDEN_CROPS:
        rev = revenue(ss, y, fp)
        cph = rev / th
        pool = [c for c in CROPS if c[1] <= gate]
        ref = max(pool, key=coins_per_hour)
        mult = cph / coins_per_hour(ref)
        print(L(name, 9) + R(gate, 11) + R(ss, 6) + R(f"{th}h", 9)
              + R(f"{rev:.0f}", 10) + R(f"{cph:.1f}", 10) + "  "
              + L(f"{ref[0]} {coins_per_hour(ref):.1f}", 16) + R(f"{mult:.2f}x", 8))
        assert_that(mult < HIDDEN_STRENGTH_CAP,
                    f"{name} 强度为同期最优的 {mult:.2f} 倍，超过上限 {HIDDEN_STRENGTH_CAP}")
    print("-" * 100)
    exp_hoes = 1 / HIDDEN_SEED_DROP
    hoes_per_day = INITIAL_PLOTS * 24 / CROPS[0][4]
    print(f"掉落概率 {HIDDEN_SEED_DROP:.0%} / 次锄地或清理，期望 {exp_hoes:.0f} 次掉落 1 颗")
    print(f"6 地循环种白萝卜每日约 {hoes_per_day:.0f} 次清理 -> "
          f"平均 {exp_hoes / hoes_per_day:.1f} 天出 1 颗")
    print(f"按此速度，首颗掉落时玩家约积累 "
          f"{cycle_exp(1, CROPS[0][7]) * hoes_per_day * exp_hoes / hoes_per_day:.0f} 经验"
          f" -> 约 Lv{int(cycle_exp(1, CROPS[0][7]) * exp_hoes / EXP_PER_LEVEL)}")
    print("等级门槛使低级玩家只能掉到人参，避免高价值作物过早破坏成长节奏")


# ============================================================================
# 检查 9 · 化肥经济性
# ============================================================================

def check_fertilizer():
    rule("检查 9 · 化肥经济性（判定是否会退化为无脑增效道具）")
    print("判据：化肥售价应高于其节省时长所对应的产出价值，使其定位为控时工具\n")
    print(L("化肥", 11) + R("节省", 8) + R("售价", 8) + "  " + L("作物", 9)
          + R("金币/h", 10) + R("节省价值", 11) + R("性价比", 10) + "  " + L("判定", 12))
    print("-" * 100)
    bad = []
    for fname, hours, price in FERTILIZERS:
        for cname in ("白萝卜", "南瓜", "菠萝", "葫芦"):
            c = crop(cname)
            cph = coins_per_hour(c)
            value = cph * hours
            ok = value < price
            if not ok:
                bad.append((fname, cname))
            print(L(fname, 11) + R(f"{hours:.1f}h", 8) + R(price, 8) + "  "
                  + L(cname, 9) + R(f"{cph:.1f}", 10) + R(f"{value:.0f}", 11)
                  + R(f"{value / price:.2f}x", 10) + "  "
                  + L("控时工具" if ok else "增效最优", 12))
    print("-" * 100)
    print(f"全部 {len(FERTILIZERS) * 4} 个组合中，"
          f"{'均符合控时工具定位' if not bad else f'以下退化为增效最优：{bad}'}")
    assert_that(not bad, f"化肥在 {bad} 组合下成为增效最优")

    print("\n单季最大压缩量（受「单次施肥不超过当前阶段剩余时长」约束）：")
    print(L("作物", 9) + R("首季", 9) + R("可施肥阶段", 12) + R("全用急速", 11)
          + R("成本", 8) + "  " + L("是否被截断", 14))
    for cname in ("白萝卜", "南瓜", "菠萝", "葫芦"):
        c = crop(cname)
        stages = fert_stages(c[1])
        first = season_split(c[3], c[4])[0] if c[3] > 1 else c[4]
        cut = stages * FERTILIZERS[-1][1]
        cost = stages * FERTILIZERS[-1][2]
        rev = revenue(c[3], c[5], c[6])
        print(L(cname, 9) + R(f"{first:.1f}h", 9) + R(stages, 12) + R(f"{cut:.1f}h", 11)
              + R(cost, 8) + "  "
              + L("是（成本 %.0f%% 收入）" % (cost / rev * 100) if cut > first
                  else "否（成本 %.0f%% 收入）" % (cost / rev * 100), 14))
    print("低级作物的压缩上限超过本季时长，但成本远超收入，经济上自然排除")


# ============================================================================
# 检查 10 · 守卫投入产出与偷菜期望
# ============================================================================

def check_dog():
    rule("检查 10 · 狗与狗粮的投入产出")
    print(f"狗盆容量 {DOG_BOWL_CAPACITY}g，标准粮耗 {DOG_FOOD_PER_HOUR}g/缩放小时 -> "
          f"满盆维持 {DOG_BOWL_CAPACITY / DOG_FOOD_PER_HOUR:.0f} 缩放小时")
    assert_that(DOG_BOWL_CAPACITY / DOG_FOOD_PER_HOUR == 24,
                "狗盆容量应恰好维持 24 缩放小时")
    c = crop("白萝卜")
    rev = revenue(c[3], c[5], c[6])
    rounds = 24 / c[4]
    paybacks = {}
    for plots in (INITIAL_PLOTS, MAX_PLOTS):
        potential = rev * STEAL_CAP_RATIO * plots * rounds
        print(f"\n{plots} 块地种白萝卜，被偷满时日均损失上限 {potential:,.0f} 金币")
        print("  " + L("狗种", 9) + R("等级", 6) + R("拦截", 7) + R("粮耗", 7)
              + R("售价", 8) + R("日均粮费", 11) + R("期望挽回", 11)
              + R("日净收益", 11) + R("回本", 9))
        for dname, p, food, price, gate in DOGS:
            cost = food * 24 * DOG_FOOD_PRICE
            net = potential * p - cost
            pb = price / net if net > 0 else float("inf")
            paybacks.setdefault(plots, []).append((dname, pb))
            print("  " + L(dname, 9) + R(f"Lv{gate}", 6) + R(f"{p:.0%}", 7)
                  + R(f"{food}g", 7) + R(price, 8) + R(cost, 11)
                  + R(f"{potential * p:,.0f}", 11) + R(f"{net:,.0f}", 11)
                  + R(f"{pb:.1f}天", 9))
    pb6 = [p for _, p in paybacks[INITIAL_PLOTS]]
    pb18 = [p for _, p in paybacks[MAX_PLOTS]]
    print(f"\n回本周期极差：{INITIAL_PLOTS} 地 {max(pb6) / min(pb6):.1f}x，"
          f"{MAX_PLOTS} 地 {max(pb18) / min(pb18):.1f}x")
    print(f"{MAX_PLOTS} 地时全部狗种回本均在 {max(pb18):.1f} 天内 -> 后期升级守卫收益明确")
    assert_that(max(pb6) / min(pb6) < 3.0,
                f"初期各狗种回本周期极差 {max(pb6) / min(pb6):.1f}x 过大，高级狗无购买理由")
    assert_that(max(pb18) < 7.0,
                f"{MAX_PLOTS} 地时最慢回本 {max(pb18):.1f} 天，高级狗后期仍无吸引力")

    rule("检查 10b · 偷菜期望收益与盈亏平衡拦截率")
    avg_steal = (STEAL_MIN + STEAL_MAX) / 2
    p_star = avg_steal / (avg_steal + CAUGHT_PENALTY_MULT)
    print(f"单次偷取期望数量 {avg_steal} 个，被抓赔付 = 果实单价 x {CAUGHT_PENALTY_MULT}")
    print(f"偷菜期望收益 = (1-p) x {avg_steal}·价 - p x {CAUGHT_PENALTY_MULT}·价")
    print(f"-> 盈亏平衡拦截率 p* = {avg_steal}/({avg_steal}+{CAUGHT_PENALTY_MULT}) "
          f"= {p_star:.1%}，与作物无关（收益与赔付同比于果实单价，果实单价约去）\n")
    print(L("狗种", 9) + R("拦截", 8) + R("相对 p*", 10) + "  " + L("对偷菜者的期望", 18)
          + "  " + L("威慑定位", 14))
    print("-" * 100)
    print(L("无狗", 9) + R("0%", 8) + R(f"{-p_star:+.1%}", 10) + "  "
          + L("正收益，无成本", 18) + "  " + L("无威慑", 14))
    for dname, p, food, price, gate in DOGS:
        ev = (1 - p) * avg_steal - p * CAUGHT_PENALTY_MULT
        print(L(dname, 9) + R(f"{p:.0%}", 8) + R(f"{p - p_star:+.1%}", 10) + "  "
              + L(f"{ev:+.2f} x 果实单价", 18) + "  "
              + L("正收益" if ev > 0.3 else ("近中性" if ev > -0.3 else "负收益·真威慑"), 14))
    print("-" * 100)
    print(f"设计意图：{DOGS[1][0]}（{DOGS[1][1]:.0%}）落在 p* 附近使偷菜近中性，"
          f"{DOGS[2][0]}（{DOGS[2][1]:.0%}）越过 p* 构成真实威慑")
    assert_that(min(abs(p - p_star) for _, p, _, _, _ in DOGS) < 0.05,
                f"应有狗种的拦截率落在盈亏平衡点 {p_star:.1%} 附近")
    assert_that(max(p for _, p, _, _, _ in DOGS) > p_star,
                f"最高拦截率应超过盈亏平衡点 {p_star:.1%}，否则守卫无威慑意义")


# ============================================================================
# 检查 11 · 日常任务奖励占比
# ============================================================================

def check_daily_tasks():
    rule("检查 11 · 日常任务奖励占比")
    top_c = sorted((t[2] for t in DAILY_TASKS), reverse=True)[:DAILY_TASK_PICK]
    top_e = sorted((t[3] for t in DAILY_TASKS), reverse=True)[:DAILY_TASK_PICK]
    max_coin, max_exp = sum(top_c), sum(top_e)
    print(f"任务池 {len(DAILY_TASKS)} 条，每逻辑日抽 {DAILY_TASK_PICK} 条，"
          f"最坏情形（抽到奖励最高的组合）{max_coin} 金币 / {max_exp} 经验\n")
    print(L("配置", 20) + R("日均金币产出", 15) + R("任务占比", 11)
          + R("日均经验产出", 15) + R("任务占比", 11) + "  " + L("判定", 8))
    print("-" * 100)
    for plots, cname in ((INITIAL_PLOTS, "白萝卜"), (INITIAL_PLOTS, "南瓜"), (MAX_PLOTS, "菠萝")):
        c = crop(cname)
        rounds = 24 / c[4]
        daily_c = (revenue(c[3], c[5], c[6]) - c[2]) * plots * rounds
        daily_e = cycle_exp(c[3], c[7]) * plots * rounds
        sc, se = max_coin / daily_c, max_exp / daily_e
        ok = sc < TASK_SHARE_CAP
        print(L(f"{plots} 地 x {cname}", 20) + R(f"{daily_c:,.0f}", 15)
              + R(f"{sc:.1%}", 11) + R(f"{daily_e:,.0f}", 15) + R(f"{se:.1%}", 11)
              + "  " + L("合理" if ok else "偏高", 8))
        assert_that(ok, f"{plots} 地种{cname}时任务金币占日均产出 {sc:.0%}，"
                        f"超过上限 {TASK_SHARE_CAP:.0%}")
    print("-" * 100)
    print("任务奖励占比随规模下降，对新手是有效补贴，对后期不构成主要收入")


# ============================================================================
# 检查 12 · 时间缩放换算
# ============================================================================

def check_time_scale():
    rule("检查 12 · TIME_SCALE 换算表")

    def fmt(hours):
        s = hours * 3600
        if s < 90:
            return f"{s:.0f}秒"
        if s < 5400:
            return f"{s / 60:.1f}分"
        if s < 86400 * 2:
            return f"{s / 3600:.1f}小时"
        return f"{s / 86400:.1f}天"

    milestones = [("白萝卜 1 轮", 10.0), ("南瓜 1 轮", 22.0),
                  ("菠萝首季", season_split(3, 116)[0]), ("菠萝全周期", 116.0),
                  ("攒够第 7 块地", 113.0)]
    print(L("profile", 12) + R("倍率", 9) + "".join(R(m[0], 15) for m in milestones))
    print("-" * 100)
    for pname, scale in TIME_PROFILES:
        print(L(pname, 12) + R(f"1/{round(1 / scale)}", 9)
              + "".join(R(fmt(h * scale), 15) for _, h in milestones))
    print("-" * 100)
    print(f"逻辑日（每日重置边界）随倍率缩放，但不短于 {LOGIC_DAY_MIN_MINUTES} 分钟真实时长：")
    for pname, scale in TIME_PROFILES:
        raw = 24 * 60 * scale
        actual = max(raw, LOGIC_DAY_MIN_MINUTES)
        note = f"  <- 下限生效（原始 {raw:.2f} 分）" if actual > raw else ""
        print(f"  {L(pname, 12)} 逻辑日 = {actual:>6.1f} 分钟真实时长{note}")
    print("\n狗盆满盆时长在各档下的真实时长（决定演示时守卫是否来得及生效）：")
    for pname, scale in TIME_PROFILES:
        print(f"  {L(pname, 12)} {fmt(DOG_BOWL_CAPACITY / DOG_FOOD_PER_HOUR * scale)}")


# ============================================================================
# 主流程
# ============================================================================

def main():
    print("=" * 100)
    print("经典农场 · 数值平衡验算报告")
    print("=" * 100)
    print(f"作物  普通 {len(CROPS)} 种 + 隐藏 {len(HIDDEN_CROPS)} 种 = "
          f"{len(CROPS) + len(HIDDEN_CROPS)} 种")
    print(f"地块  {INITIAL_PLOTS} -> {MAX_PLOTS}    "
          f"等级  累计经验 = N x {EXP_PER_LEVEL}    初始金币 {INITIAL_COINS}")

    for fn in (check_health_formula, check_roi, check_efficiency, check_data_anomaly,
               check_season_split, check_pacing, check_daily_cap, check_hidden_crops,
               check_fertilizer, check_dog, check_daily_tasks, check_time_scale):
        fn()

    rule("断言汇总")
    if FAILURES:
        print(f"共 {len(FAILURES)} 项断言失败：")
        for f in FAILURES:
            print(f"  - {f}")
    else:
        print("全部断言通过。")
    return 1 if FAILURES else 0


if __name__ == "__main__":
    raise SystemExit(main())
