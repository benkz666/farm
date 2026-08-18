#!/usr/bin/env python3
"""Generate the exact-data chart embedded in performance slide 3.3."""

from pathlib import Path


OUT = Path(__file__).resolve().parents[1] / "assets" / "figures" / "slide_03_results.svg"

NAVY = "#0b1838"
BLUE = "#2f66e9"
TEAL = "#00a58f"
ORANGE = "#ff721b"
GRID = "#dce7f7"
TRACK = "#edf3fb"
BORDER = "#9ab8ff"


def text(x, y, value, size=24, weight=400, color=NAVY, anchor="start"):
    return (
        f'<text x="{x}" y="{y}" font-family="DejaVu Sans, sans-serif" '
        f'font-size="{size}" font-weight="{weight}" fill="{color}" '
        f'text-anchor="{anchor}">{value}</text>'
    )


def rect(x, y, w, h, fill="none", stroke="none", sw=1, rx=0):
    return (
        f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{rx}" '
        f'fill="{fill}" stroke="{stroke}" stroke-width="{sw}"/>'
    )


def line(x1, y1, x2, y2, stroke, sw=2, dash=""):
    dash_attr = f' stroke-dasharray="{dash}"' if dash else ""
    return (
        f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" '
        f'stroke="{stroke}" stroke-width="{sw}"{dash_attr}/>'
    )


def generate():
    width, height = 1800, 780
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">',
        rect(0, 0, width, height, "#ffffff"),
        rect(15, 15, 855, 750, "#ffffff", BORDER, 2, 8),
        rect(930, 15, 855, 750, "#ffffff", BORDER, 2, 8),
        text(55, 70, "01  SLO margin", 30, 700),
        text(970, 70, "02  Service CPU utilization", 30, 700),
    ]

    # Left: actual metric as percentage of its SLO limit.
    slo_rows = [
        ("P90 latency", 55.57 / 300 * 100, "55.57ms / 300ms"),
        ("P99 latency", 284.62 / 500 * 100, "284.62ms / 500ms"),
        ("Max error rate", 0.00293 / 0.1 * 100, "0.00293% / 0.1%"),
        ("Async drain", 47.699 / 60 * 100, "47.699s / 60s"),
    ]
    x0, track_w = 255, 520
    threshold_x = x0 + track_w
    parts += [
        line(threshold_x, 120, threshold_x, 690, ORANGE, 3, "10 9"),
        text(threshold_x, 110, "100% SLO", 18, 600, ORANGE, "middle"),
    ]
    for i, (label, ratio, value_label) in enumerate(slo_rows):
        y = 175 + i * 140
        bar_y = y + 25
        parts += [
            text(55, y, label, 25, 600),
            text(55, y + 38, value_label, 20, 400, "#60708f"),
            rect(x0, bar_y, track_w, 32, TRACK, "none", rx=16),
            rect(x0, bar_y, max(8, track_w * ratio / 100), 32, TEAL, "none", rx=16),
            text(x0 + max(8, track_w * ratio / 100) + 12, bar_y + 25, f"{ratio:.1f}%", 21, 700, TEAL),
        ]

    # Right: average and peak CPU, plus 70% planning line.
    services = ["Gateway", "Farm", "Social", "MySQL", "Redis"]
    averages = [47.53, 45.90, 45.52, 31.23, 35.65]
    peaks = [50.49, 49.65, 47.93, 39.96, 45.85]
    chart_x, chart_y, chart_w, chart_h = 1015, 145, 700, 510
    max_y = 80
    for tick in [0, 20, 40, 60, 80]:
        y = chart_y + chart_h - chart_h * tick / max_y
        parts += [
            line(chart_x, y, chart_x + chart_w, y, GRID, 1),
            text(chart_x - 18, y + 7, str(tick), 18, 400, "#60708f", "end"),
        ]
    planning_y = chart_y + chart_h - chart_h * 70 / max_y
    parts += [
        line(chart_x, planning_y, chart_x + chart_w, planning_y, ORANGE, 3, "12 10"),
        text(chart_x + chart_w - 4, planning_y - 12, "70% planning level", 18, 600, ORANGE, "end"),
        rect(1180, 88, 22, 14, BLUE),
        text(1212, 101, "Average", 18, 500),
        rect(1345, 88, 22, 14, TEAL),
        text(1377, 101, "30s peak", 18, 500),
    ]
    group_w = chart_w / len(services)
    bar_w = 38
    for i, service in enumerate(services):
        center = chart_x + group_w * (i + 0.5)
        avg_h = chart_h * averages[i] / max_y
        peak_h = chart_h * peaks[i] / max_y
        avg_x = center - bar_w - 5
        peak_x = center + 5
        parts += [
            rect(avg_x, chart_y + chart_h - avg_h, bar_w, avg_h, BLUE),
            rect(peak_x, chart_y + chart_h - peak_h, bar_w, peak_h, TEAL),
            text(avg_x + bar_w / 2, chart_y + chart_h - avg_h - 10, f"{averages[i]:.1f}%", 17, 600, NAVY, "middle"),
            text(peak_x + bar_w / 2, chart_y + chart_h - peak_h - 10, f"{peaks[i]:.1f}%", 17, 600, NAVY, "middle"),
            text(center, chart_y + chart_h + 38, service, 20, 500, "#60708f", "middle"),
        ]

    parts.append("</svg>")
    OUT.write_text("\n".join(parts), encoding="utf-8")


if __name__ == "__main__":
    generate()
