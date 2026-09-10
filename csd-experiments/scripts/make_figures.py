#!/usr/bin/env python3
"""Render the thesis figures from the collected experiment CSVs.

matplotlib is not available in this environment, so the figures are drawn with
Pillow. Labels are in English because no CJK font is installed; the thesis
captions are in Chinese.
"""

import csv
import math
import os
import statistics
import sys

from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS = os.path.join(HERE, "..", "results")
FIGURES = os.path.join(RESULTS, "figures")

WIDTH, HEIGHT = 1000, 620
MARGIN_LEFT, MARGIN_RIGHT, MARGIN_TOP, MARGIN_BOTTOM = 110, 40, 70, 110

PALETTE = [(52, 101, 164), (196, 78, 82), (120, 160, 90), (150, 110, 180)]
GRID = (225, 228, 232)
AXIS = (90, 95, 100)


def font(size, bold=False):
    candidates = [
        "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf" if bold
        else "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    ]
    for path in candidates:
        if os.path.exists(path):
            return ImageFont.truetype(path, size)
    return ImageFont.load_default()


def load(name):
    with open(os.path.join(RESULTS, name)) as handle:
        return list(csv.DictReader(handle))


def medians(rows, phase_filter, value_key=None):
    """Median of a latency column; the read-path CSV names it total_ms, the
    search CSVs name it latency_ms."""
    values = []
    for row in rows:
        if not phase_filter(row):
            continue
        for key in ([value_key] if value_key else ["latency_ms", "total_ms"]):
            if key in row and row[key] not in ("", None):
                values.append(float(row[key]))
                break
    return statistics.median(values) if values else 0.0


def new_canvas(title, ylabel, ymax):
    image = Image.new("RGB", (WIDTH, HEIGHT), "white")
    draw = ImageDraw.Draw(image)
    draw.text((MARGIN_LEFT, 26), title, fill=(20, 20, 20), font=font(24, True))
    plot_w = WIDTH - MARGIN_LEFT - MARGIN_RIGHT
    plot_h = HEIGHT - MARGIN_TOP - MARGIN_BOTTOM

    ticks = 5
    for i in range(ticks + 1):
        y = MARGIN_TOP + plot_h - plot_h * i / ticks
        draw.line([(MARGIN_LEFT, y), (MARGIN_LEFT + plot_w, y)], fill=GRID)
        value = ymax * i / ticks
        draw.text((MARGIN_LEFT - 74, y - 9), f"{value:.0f}", fill=AXIS, font=font(16))
    draw.line([(MARGIN_LEFT, MARGIN_TOP), (MARGIN_LEFT, MARGIN_TOP + plot_h)], fill=AXIS, width=2)
    draw.line([(MARGIN_LEFT, MARGIN_TOP + plot_h), (MARGIN_LEFT + plot_w, MARGIN_TOP + plot_h)],
              fill=AXIS, width=2)
    draw.text((16, MARGIN_TOP - 34), ylabel, fill=AXIS, font=font(16, True))
    return image, draw, plot_w, plot_h


def grouped_bars(path, groups, series, title, ylabel, ymax=None, fmt="{:.0f}"):
    """series: list of (label, [value per group])."""
    top = ymax or max(max(values) for _, values in series) * 1.15
    image, draw, plot_w, plot_h = new_canvas(title, ylabel, top)
    slot = plot_w / len(groups)
    bar_w = slot / (len(series) + 0.8)

    for index, (label, values) in enumerate(series):
        colour = PALETTE[index % len(PALETTE)]
        for g, value in enumerate(values):
            x0 = MARGIN_LEFT + g * slot + slot * 0.12 + index * bar_w
            height = plot_h * (value / top)
            y0 = MARGIN_TOP + plot_h - height
            draw.rectangle([x0, y0, x0 + bar_w * 0.86, MARGIN_TOP + plot_h], fill=colour)
            draw.text((x0 - 2, y0 - 22), fmt.format(value), fill=(40, 40, 40), font=font(15, True))
        draw.rectangle([MARGIN_LEFT + 12 + index * 220, HEIGHT - 74,
                        MARGIN_LEFT + 34 + index * 220, HEIGHT - 58], fill=colour)
        draw.text((MARGIN_LEFT + 40 + index * 220, HEIGHT - 78), label,
                  fill=(30, 30, 30), font=font(16))

    for g, label in enumerate(groups):
        centre = MARGIN_LEFT + g * slot + slot / 2
        draw.text((centre - len(label) * 5, MARGIN_TOP + plot_h + 16), label,
                  fill=(30, 30, 30), font=font(18))
    image.save(path)
    print("wrote", path)


def line_chart(path, xs, series, title, xlabel, ylabel, log_y=False):
    """series: list of (label, [value per x])."""
    finite = [v for _, values in series for v in values if v > 0]
    lo = min(finite) if finite else 1.0
    hi = max(finite) if finite else 1.0
    if log_y:
        lo, hi = math.log10(lo), math.log10(hi)
    span = (hi - lo) or 1.0
    lo, hi = lo - span * 0.15, hi + span * 0.15

    def project(value):
        if log_y:
            value = math.log10(max(value, 0.001))
        return (value - lo) / (hi - lo)

    image, draw, plot_w, plot_h = new_canvas(title, ylabel, 1.0)
    ticks = 5
    for i in range(ticks + 1):
        y = MARGIN_TOP + plot_h - plot_h * i / ticks
        raw = lo + (hi - lo) * i / ticks
        text = f"{10 ** raw:.0f}" if log_y else f"{raw:.0f}"
        draw.text((MARGIN_LEFT - 74, y - 9), text, fill=AXIS, font=font(16))

    def point(index, value):
        x = MARGIN_LEFT + 30 + (plot_w - 60) * (index / max(len(xs) - 1, 1))
        y = MARGIN_TOP + plot_h - plot_h * project(value)
        return x, y

    for index, (label, values) in enumerate(series):
        colour = PALETTE[index % len(PALETTE)]
        points = [point(i, v) for i, v in enumerate(values)]
        draw.line(points, fill=colour, width=3)
        for x, y in points:
            draw.ellipse([x - 5, y - 5, x + 5, y + 5], fill=colour)
        draw.rectangle([MARGIN_LEFT + 12 + index * 250, HEIGHT - 74,
                        MARGIN_LEFT + 34 + index * 250, HEIGHT - 58], fill=colour)
        draw.text((MARGIN_LEFT + 40 + index * 250, HEIGHT - 78), label,
                  fill=(30, 30, 30), font=font(16))
    for i, label in enumerate(xs):
        x, _ = point(i, series[0][1][i])
        draw.text((x - len(label) * 5, MARGIN_TOP + plot_h + 16), label,
                  fill=(30, 30, 30), font=font(18))
    draw.text((WIDTH // 2 - len(xlabel) * 5, HEIGHT - 34), xlabel,
              fill=(30, 30, 30), font=font(17, True))
    image.save(path)
    print("wrote", path)


def main():
    os.makedirs(FIGURES, exist_ok=True)

    # 图 6-1 读路径时延(冷/热)
    rows = load("phase1_local.csv")
    warm = [medians(rows, lambda r, m=m: r["mode"] == m and r["cache"] == "warm")
            for m in ("legacy", "aligned", "direct")]
    cold = [medians(rows, lambda r, m=m: r["mode"] == m and r["cache"] == "cold")
            for m in ("legacy", "aligned", "direct")]
    grouped_bars(os.path.join(FIGURES, "fig6-1_read_path.png"),
                 ["legacy", "aligned", "direct"],
                 [("page cache warm", warm), ("page cache cold", cold)],
                 "Figure 6-1  Compute latency by read path (64 MiB, 16 chunks)",
                 "median latency (ms)")

    # 图 6-2 自适应策略
    rows = load("phase1_cache_policy.csv")
    warm = [medians(rows, lambda r, m=m: r["mode"] == m and r["cache"] == "warm")
            for m in ("direct", "auto")]
    cold = [medians(rows, lambda r, m=m: r["mode"] == m and r["cache"] == "cold")
            for m in ("direct", "auto")]
    grouped_bars(os.path.join(FIGURES, "fig6-2_adaptive.png"),
                 ["always O_DIRECT", "adaptive"],
                 [("cache warm", warm), ("cache cold", cold)],
                 "Figure 6-2  Adaptive data path vs always-O_DIRECT",
                 "median latency (ms)")

    # 图 6-3 图像检索规模曲线
    points = []
    for name, images in [("image_search_1024_k5.csv", 1024),
                         ("image_search.csv", 4096),
                         ("image_search_16384_k5.csv", 16384)]:
        rows = load(name)
        near = medians(rows, lambda r: r["phase"] == "near-storage")
        base = medians(rows, lambda r: r["phase"] != "near-storage")
        points.append((images, near, base))
    xs = [f"{p[0]}\n({p[0] * 4096 // (1 << 20)} MiB)" for p in points]
    line_chart(os.path.join(FIGURES, "fig6-3_search_scale.png"),
               xs,
               [("near-storage search", [p[1] for p in points]),
                ("download + client search", [p[2] for p in points])],
               "Figure 6-3  Image search: latency vs gallery size",
               "gallery size (images)", "median latency (ms, log scale)", log_y=True)

    # 图 6-4 回传数据量与图库规模解耦
    returned = []
    transferred = []
    for name, _ in [("image_search_1024_k5.csv", 1024), ("image_search.csv", 4096),
                    ("image_search_16384_k5.csv", 16384)]:
        rows = load(name)
        returned.append(float([r for r in rows if r["phase"] == "near-storage"][0]["bytes_returned"]))
        transferred.append(float([r for r in rows if r["phase"] != "near-storage"][0]["bytes_returned"]))
    line_chart(os.path.join(FIGURES, "fig6-4_traffic.png"),
               xs,
               [("bytes returned (near-storage)", returned),
                ("bytes transferred (baseline)", transferred)],
               "Figure 6-4  Response size is decoupled from gallery size",
               "gallery size (images)", "bytes (log scale)", log_y=True)

    # 图 6-5 描述子对比
    grouped_bars(os.path.join(FIGURES, "fig6-5_descriptor.png"),
                 ["histogram+quadrant", "8x8 grid", "histogram+grid"],
                 [("precision@5", [0.360, 0.824, 0.822])],
                 "Figure 6-5  Descriptor quality on real images (MNIST, 2048 gallery)",
                 "mean precision@5", ymax=1.0, fmt="{:.3f}")

    # 图 6-6 真实数据集:时延与精度
    sizes, near_ms, base_ms, precision = [], [], [], []
    for name, images, prec in [("image_search_mnist_4096.csv", 4096, 0.860),
                               ("image_search_mnist_8192.csv", 8192, 0.947)]:
        rows = load(name)
        near_ms.append(medians(rows, lambda r: r["phase"] == "near-storage"))
        base_ms.append(medians(rows, lambda r: r["phase"] != "near-storage"))
        sizes.append(f"{images}\n({images * 4096 // (1 << 20)} MiB)")
        precision.append(prec)
    line_chart(os.path.join(FIGURES, "fig6-6_mnist.png"),
               sizes,
               [("near-storage search", near_ms),
                ("download + client search", base_ms)],
               "Figure 6-6  Real image dataset (MNIST): latency vs gallery size",
               "gallery size (real digits)", "median latency (ms, log scale)", log_y=True)

    grouped_bars(os.path.join(FIGURES, "fig6-7_mnist_precision.png"),
                 ["4096 images", "8192 images"],
                 [("precision@5 (30 held-out queries)", precision)],
                 "Figure 6-7  Retrieval quality on real images",
                 "mean precision@5", ymax=1.0, fmt="{:.3f}")


if __name__ == "__main__":
    sys.exit(main())
