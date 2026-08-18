from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
from matplotlib import font_manager
from matplotlib.ticker import FuncFormatter


OUTPUT_DIR = Path(__file__).resolve().parent / "assets"
OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

FONT_CANDIDATES = (
    Path("/tmp/NotoSansCJKsc-Regular.otf"),
    Path("/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"),
    Path("/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc"),
)
FONT_FAMILY = "DejaVu Sans"
for candidate in FONT_CANDIDATES:
    if candidate.exists():
        font_manager.fontManager.addfont(candidate)
        FONT_FAMILY = font_manager.FontProperties(fname=candidate).get_name()
        break
bold_font = Path("/tmp/NotoSansCJKsc-Bold.otf")
if bold_font.exists():
    font_manager.fontManager.addfont(bold_font)

NAVY = "#071A3D"
BLUE = "#2864F0"
BLUE_DARK = "#164FD8"
TEAL = "#00A58E"
ORANGE = "#FF7A1A"
LINE = "#BBD0FF"
GRID = "#E5ECF8"
MUTED = "#5D6B86"
WHITE = "#FFFFFF"

plt.rcParams.update(
    {
        "font.family": FONT_FAMILY,
        "font.size": 10,
        "axes.titlesize": 13,
        "axes.labelsize": 10.5,
        "axes.edgecolor": LINE,
        "axes.linewidth": 1.0,
        "axes.grid": True,
        "grid.color": GRID,
        "grid.linewidth": 0.8,
        "grid.alpha": 1.0,
        "xtick.color": MUTED,
        "ytick.color": NAVY,
        "text.color": NAVY,
        "figure.facecolor": WHITE,
        "axes.facecolor": WHITE,
        "savefig.facecolor": WHITE,
        "axes.unicode_minus": False,
    }
)


def decorate_axis(ax, title):
    ax.set_title(title, loc="left", pad=14, fontweight="bold", color=NAVY)
    for spine in ax.spines.values():
        spine.set_color(LINE)
        spine.set_linewidth(1.0)
    ax.tick_params(length=0)
def plot_interface_performance():
    labels = [
        "Handshake · 建连",
        "Ping · 心跳",
        "EnterFarm · Actor热",
        "EnterFarm · Actor冷",
        "SyncFarm · 热同步",
        "Water · 本地",
        "Harvest · 收获",
        "Buy · 购买",
        "Sell · 出售",
        "Steal · 跨农场",
        "Water · 跨农场",
        "SearchUser · 缓存热",
        "SearchUser · 缓存冷",
        "FriendList · 缓存热",
        "FriendList · 缓存冷",
        "AreFriends · 缓存热",
        "AreFriends · 缓存冷",
        "TaskList · 缓存热",
        "TaskList · Actor冷",
        "MailList · 缓存热",
        "MailList · Actor冷",
    ]
    throughput = np.array(
        [12670, 87844, 43201, 10830, 79716, 15859, 11962, 15611, 13944, 6780,
         7723, 79087, 20218, 54458, 13749, 51421, 14937, 39870, 8496, 49518, 9714]
    )
    average = np.array(
        [68.0, 55.4, 47.3, 95.59, 63.7, 40.1, 57.2, 57.2, 37.1, 46.9, 46.2,
         57.3, 54.0, 36.4, 50.1, 53.2, 36.1, 37.6, 39.4, 45.3, 58.3]
    )
    p90 = np.array(
        [121.3, 136.9, 122.6, 140.61, 96.0, 136.2, 133.4, 123.4, 101.8,
         104.3, 95.5, 132.0, 124.5, 108.1, 105.5, 119.6, 107.0, 132.1,
         87.3, 131.5, 99.5]
    )
    p99 = np.array(
        [182.9, 180.7, 146.0, 255.60, 203.6, 237.1, 173.8, 164.4, 175.3,
         164.6, 159.6, 174.8, 193.3, 190.3, 174.0, 164.3, 232.4, 213.9,
         171.7, 165.1, 228.9]
    )

    y = np.arange(len(labels))
    fig, (ax_qps, ax_latency) = plt.subplots(
        1,
        2,
        figsize=(18, 11.5),
        sharey=True,
        gridspec_kw={"width_ratios": [1.04, 1.08], "wspace": 0.08},
    )
    fig.suptitle("单接口稳定吞吐与时延", fontsize=19, fontweight="bold", color=NAVY, y=0.985)

    cold_mask = np.array(["冷" in label for label in labels])
    colors = np.where(cold_mask, TEAL, BLUE)
    bars = ax_qps.barh(y, throughput, height=0.58, color=colors, edgecolor="none")
    ax_qps.set_yticks(y, labels)
    ax_qps.invert_yaxis()
    decorate_axis(ax_qps, "01  稳定吞吐")
    ax_qps.set_xlabel("请求速率（QPS；Handshake为连接/s）", color=MUTED, labelpad=10)
    ax_qps.xaxis.set_major_formatter(FuncFormatter(lambda value, _: f"{value / 1000:.0f}k"))
    ax_qps.grid(axis="x")
    ax_qps.grid(axis="y", visible=False)
    ax_qps.set_xlim(0, throughput.max() * 1.18)
    for bar in bars:
        value = bar.get_width()
        ax_qps.text(
            value + throughput.max() * 0.012,
            bar.get_y() + bar.get_height() / 2,
            f"{value:,.0f}",
            va="center",
            ha="left",
            fontsize=8.5,
            color=NAVY,
            fontweight="bold",
        )

    ax_latency.scatter(average, y, s=42, color=TEAL, label="Avg", zorder=3)
    ax_latency.scatter(p90, y, s=42, color=BLUE, label="P90", zorder=3)
    ax_latency.scatter(p99, y, s=42, color=ORANGE, label="P99", zorder=3)
    for row in range(len(labels)):
        ax_latency.plot(
            [average[row], p99[row]], [row, row], color="#D5E0F4", linewidth=1.4, zorder=1
        )
    ax_latency.axvline(100, color=BLUE, linestyle=(0, (4, 3)), linewidth=1, alpha=0.55)
    ax_latency.axvline(200, color=BLUE_DARK, linestyle=(0, (4, 3)), linewidth=1, alpha=0.55)
    ax_latency.text(100, -0.85, "100ms", ha="center", va="bottom", fontsize=8, color=BLUE)
    ax_latency.text(200, -0.85, "200ms", ha="center", va="bottom", fontsize=8, color=BLUE_DARK)
    decorate_axis(ax_latency, "02  稳定档响应时延")
    ax_latency.set_xlabel("响应时延（ms）", color=MUTED, labelpad=10)
    ax_latency.set_xlim(0, max(p99) * 1.12)
    ax_latency.grid(axis="x")
    ax_latency.grid(axis="y", visible=False)
    legend = ax_latency.legend(loc="lower right", ncol=3, frameon=False, columnspacing=1.5)
    for text in legend.get_texts():
        text.set_color(NAVY)

    fig.subplots_adjust(left=0.185, right=0.98, top=0.94, bottom=0.075)
    fig.savefig(
        OUTPUT_DIR / "single-interface-performance.png", dpi=180, bbox_inches="tight", pad_inches=0.16
    )
    plt.close(fig)


def plot_capacity_resource_usage():
    services = ["Gateway", "Farm", "Social", "MySQL", "Redis"]
    quota = np.array([5.0, 5.0, 1.0, 3.0, 3.0])
    average_cpu = np.array([2.3766, 2.2950, 0.4552, 0.9370, 1.0695])
    peak_cpu = np.array([2.5247, 2.4827, 0.4793, 1.1987, 1.3754])
    average_utilization = average_cpu / quota * 100
    peak_utilization = peak_cpu / quota * 100

    x = np.arange(len(services))
    width = 0.32
    fig, (ax_cores, ax_utilization) = plt.subplots(
        1,
        2,
        figsize=(16, 8.4),
        gridspec_kw={"wspace": 0.14},
    )
    fig.suptitle(
        "10k容量单元各服务CPU使用情况",
        fontsize=19,
        fontweight="bold",
        color=NAVY,
        y=0.985,
    )

    avg_bars = ax_cores.bar(
        x - width / 2, average_cpu, width, color=BLUE, edgecolor="none", label="平均CPU"
    )
    peak_bars = ax_cores.bar(
        x + width / 2, peak_cpu, width, color=TEAL, edgecolor="none", label="30秒平滑峰值"
    )
    ax_cores.plot(
        x, quota, color=MUTED, marker="o", markersize=5, linestyle=(0, (4, 3)), label="实验配额"
    )
    decorate_axis(ax_cores, "01  实际CPU核数")
    ax_cores.set_ylabel("CPU核数", color=MUTED)
    ax_cores.set_xticks(x, services)
    ax_cores.set_ylim(0, quota.max() * 1.16)
    ax_cores.grid(axis="y")
    ax_cores.grid(axis="x", visible=False)
    ax_cores.bar_label(avg_bars, fmt="%.2f", padding=4, fontsize=9, color=NAVY)
    ax_cores.bar_label(peak_bars, fmt="%.2f", padding=4, fontsize=9, color=NAVY)
    legend = ax_cores.legend(frameon=False, ncol=3, loc="upper right")
    for text in legend.get_texts():
        text.set_color(NAVY)

    avg_pct_bars = ax_utilization.bar(
        x - width / 2,
        average_utilization,
        width,
        color=BLUE,
        edgecolor="none",
        label="平均利用率",
    )
    peak_pct_bars = ax_utilization.bar(
        x + width / 2,
        peak_utilization,
        width,
        color=TEAL,
        edgecolor="none",
        label="峰值利用率",
    )
    ax_utilization.axhline(
        70, color=ORANGE, linestyle=(0, (5, 3)), linewidth=1.7, label="70%规划水位"
    )
    decorate_axis(ax_utilization, "02  相对实验配额的利用率")
    ax_utilization.set_ylabel("利用率（%）", color=MUTED)
    ax_utilization.set_xticks(x, services)
    ax_utilization.set_ylim(0, 82)
    ax_utilization.grid(axis="y")
    ax_utilization.grid(axis="x", visible=False)
    ax_utilization.bar_label(avg_pct_bars, fmt="%.1f%%", padding=4, fontsize=9, color=NAVY)
    ax_utilization.bar_label(peak_pct_bars, fmt="%.1f%%", padding=4, fontsize=9, color=NAVY)
    legend = ax_utilization.legend(frameon=False, ncol=3, loc="upper right")
    for text in legend.get_texts():
        text.set_color(NAVY)

    fig.subplots_adjust(left=0.07, right=0.98, top=0.90, bottom=0.10)
    fig.savefig(
        OUTPUT_DIR / "capacity-unit-cpu-usage.png", dpi=180, bbox_inches="tight", pad_inches=0.16
    )
    plt.close(fig)


if __name__ == "__main__":
    plot_interface_performance()
    plot_capacity_resource_usage()
