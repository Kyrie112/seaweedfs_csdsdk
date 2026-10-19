#!/usr/bin/env python3
"""Fit the offload decision cost model from the collected experiment data.

The model (see the phase-2 report):

    T_host(S) = L0_host + S / B_link + S / P_host
    T_off(S)  = L0_off  + S / B_dev  + S / P_dev

Both paths are linear in the data size S. The measured end-to-end latencies in
the image-search CSVs are used to fit the slopes and intercepts; the script then
reports the break-even condition and the host compute rate above which the host
path becomes competitive.
"""

import csv
import os
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS = os.path.join(HERE, "..", "results")

# (csv, images) -> gallery bytes = images * 4096
DATASETS = [
    ("image_search_1024_k5.csv", 1024),
    ("image_search.csv", 4096),
    ("image_search_16384_k5.csv", 16384),
]


def load_points():
    points = []
    for name, images in DATASETS:
        path = os.path.join(RESULTS, name)
        rows = list(csv.DictReader(open(path)))
        near = [float(r["latency_ms"]) for r in rows if r["phase"] == "near-storage"]
        base = [float(r["latency_ms"]) for r in rows if r["phase"] != "near-storage"][0]
        points.append((images * 4096 / (1 << 20), statistics.median(near), base))
    return points


def fit(xs, ys):
    """Least-squares line y = a*x + b."""
    n = len(xs)
    mean_x = sum(xs) / n
    mean_y = sum(ys) / n
    num = sum((x - mean_x) * (y - mean_y) for x, y in zip(xs, ys))
    den = sum((x - mean_x) ** 2 for x in xs)
    slope = num / den if den else 0.0
    intercept = mean_y - slope * mean_x
    # With only three points the unconstrained fit can produce a small negative
    # fixed cost; clamp it to zero (a non-negative fixed cost is the physically
    # meaningful model, and the slope is what the break-even analysis uses).
    if intercept < 0:
        slope = sum(x * y for x, y in zip(xs, ys)) / sum(x * x for x in xs)
        intercept = 0.0
    return slope, intercept


def main():
    points = load_points()
    print("measured points (MiB, near-storage ms, host-path ms)")
    for size, near, host in points:
        print(f"  {size:6.1f} MiB   {near:9.2f}   {host:9.2f}")

    xs = [p[0] for p in points]
    near_slope, near_intercept = fit(xs, [p[1] for p in points])
    host_slope, host_intercept = fit(xs, [p[2] for p in points])
    print()
    print(f"T_off(S)  = {near_intercept:7.2f} ms + {near_slope:8.4f} ms/MiB * S")
    print(f"T_host(S) = {host_intercept:7.2f} ms + {host_slope:8.4f} ms/MiB * S")

    # Break-even: T_host < T_off  <=>  (host_slope - near_slope) * S < near_intercept - host_intercept
    slope_gap = host_slope - near_slope
    intercept_gap = near_intercept - host_intercept
    print()
    if slope_gap > 0:
        print(f"crossover S* = {intercept_gap / slope_gap:.1f} MiB "
              f"(host wins above this size)")
    else:
        print("no positive crossover in the measured region: the offload slope is "
              "not larger than the host slope, so the host does not overtake it by size alone")

    # The host slope is 1000/B_link + 1000/P_host (ms/MiB). Report the compute
    # rate above which the host path would beat the measured offload slope.
    print()
    print("host compute rate P at which the host slope equals the offload slope:")
    print(f"{'B_link (MiB/s)':>16} {'P_critical (MiB/s)':>20}")
    for link in (100, 500, 1000, 5000, 10000):
        # 1000/link + 1000/P = near_slope
        rest = near_slope - 1000.0 / link
        if rest <= 0:
            print(f"{link:>16} {'host already competitive':>20}")
        else:
            print(f"{link:>16} {1000.0 / rest:>20.0f}")

    # Sensitivity: per-size latency for representative (link, compute) pairs.
    print()
    print("predicted host-path latency (ms) for representative parameters")
    print(f"{'B_link':>8} {'P_host':>8} " + " ".join(f"{f'{s:g}MiB':>10}" for s in xs))
    for link, compute in ((1000, 500), (1000, 2000), (5000, 5000)):
        row = []
        for size in xs:
            row.append(f"{host_intercept + size * (1000.0 / link + 1000.0 / compute):10.1f}")
        print(f"{link:>8} {compute:>8} " + " ".join(row))


if __name__ == "__main__":
    sys.exit(main())
