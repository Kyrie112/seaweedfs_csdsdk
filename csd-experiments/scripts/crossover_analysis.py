#!/usr/bin/env python3
"""Crossover analysis: when is host-side compute better than CSD-side compute?

The marginal cost per MiB is the decisive quantity:

    host:  a_h = 1/B_link + 1/P_host
    csd:   a_c = 1/(k_read*B_dev) + 1/(k_compute*P_dev)

A crossover exists when a_h > a_c:

    S* = (L_csd - L_host) / (a_h - a_c)

Below S* the host wins (fixed overheads dominate), above S* the CSD wins.
If a_h <= a_c the host wins for every size; if the CSD also has a lower fixed
cost, the CSD wins everywhere.

The script estimates B_link, P_host (Python client), B_dev and P_dev from the
experiment CSVs and then classifies the realistic regimes.
"""

import csv
import os
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS = os.path.join(HERE, "..", "results")
MIB = 1 << 20

DATASETS = [
    ("image_search_1024_k5.csv", 1024, 1),
    ("image_search.csv", 4096, 4),
    ("image_search_16384_k5.csv", 16384, 16),
]


def load():
    points = []
    for name, images, chunks in DATASETS:
        rows = list(csv.DictReader(open(os.path.join(RESULTS, name))))
        near = [r for r in rows if r["phase"] == "near-storage"]
        base = [r for r in rows if r["phase"] != "near-storage"][0]
        size = images * 4096 / MIB
        download_ms = float(base["dispatch_ms"])
        client_ms = float(base["kernel_ms"])
        # device_read_ms is the sum over chunks; per-chunk time = sum/chunks
        read_per_chunk_ms = statistics.median(
            [float(r["device_read_ms"]) for r in near]) / chunks
        kernel_per_chunk_ms = statistics.median(
            [float(r["kernel_ms"]) for r in near]) / chunks
        points.append({
            "size": size, "chunks": chunks,
            "link": size / (download_ms / 1000.0),
            "p_host": size / (client_ms / 1000.0),
            "b_dev": (size / chunks) / (read_per_chunk_ms / 1000.0),
            "p_dev": (size / chunks) / (kernel_per_chunk_ms / 1000.0),
        })
    return points


def crossover(l_host, l_csd, a_host, a_csd):
    if a_host <= a_csd:
        return None, "host wins for all sizes"
    if l_csd <= l_host:
        return 0.0, "csd wins for all sizes"
    return (l_csd - l_host) / (a_host - a_csd), "crossover"


def main():
    points = load()
    print("measured components (MiB/s)")
    print(f"{'size':>6} {'chunks':>6} {'B_link':>9} {'P_host':>9} {'B_dev/chunk':>12} {'P_dev/chunk':>12}")
    for p in points:
        print(f"{p['size']:6.1f} {p['chunks']:6d} {p['link']:9.1f} "
              f"{p['p_host']:9.1f} {p['b_dev']:12.1f} {p['p_dev']:12.1f}")

    link = statistics.median([p["link"] for p in points])
    p_host_py = statistics.median([p["p_host"] for p in points])
    b_dev = statistics.median([p["b_dev"] for p in points])
    p_dev = statistics.median([p["p_dev"] for p in points])
    print(f"\nmedian: B_link={link:.1f}  P_host(python)={p_host_py:.1f}  "
          f"B_dev/chunk={b_dev:.1f}  P_dev/chunk={p_dev:.1f} MiB/s")

    # Fixed overheads: from the image-search fits (both ~9 ms) and the download fit.
    l_host, l_csd = 8.7, 8.7

    print("\n=== regime comparison (fixed: B_link=%.0f, B_dev=%.0f, P_dev=%.0f) ===" % (link, b_dev, p_dev))
    print(f"{'P_host':>8} {'CSD mode':>22} {'a_host':>8} {'a_csd':>8} {'S* (MiB)':>10}  verdict")
    modes = [
        ("serial (k=1)", 1, 1),
        ("concurrent sim (k=8)", 8, 8),
        ("pipelined (read=8,compute=1)", 8, 1),
    ]
    for p in (p_host_py, 50, 100, 200, 500, 1000, 2000):
        a_h = 1.0 / link + 1.0 / p  # seconds per MiB
        for label, k_read, k_compute in modes:
            a_c = 1.0 / (k_read * b_dev) + 1.0 / (k_compute * p_dev)
            s_star, verdict = crossover(l_host, l_csd, a_h, a_c)
            s_text = f"{s_star:10.2f}" if s_star is not None else f"{'-':>10}"
            print(f"{p:8.0f} {label:>22} {a_h*1000:8.2f} {a_c*1000:8.2f} {s_text}  {verdict}")

    print("\n=== decision matrix (rows: link bandwidth, cols: host compute rate) ===")
    hosts = [50, 100, 200, 500, 1000, 2000]
    links = [100, 300, 1000, 3000, 10000]
    print("entry = S* (MiB) above which CSD wins; 'H' = host always; 'C' = CSD always")
    print(f"{'B_link':>8} " + " ".join(f"{h:>10}" for h in hosts))
    for l in links:
        row = []
        for h in hosts:
            a_h = 1.0 / l + 1.0 / h
            a_c = 1.0 / (8 * b_dev) + 1.0 / (8 * p_dev)  # concurrent CSD
            s_star, verdict = crossover(l_host, l_csd, a_h, a_c)
            if verdict.startswith("host"):
                row.append(f"{'H':>10}")
            elif s_star == 0:
                row.append(f"{'C':>10}")
            else:
                row.append(f"{s_star:10.2f}")
        print(f"{l:>8} " + " ".join(row))

    print("\nnote: 'concurrent' assumes k parallel chunk reads and kernels; a serial")
    print("      device agent (the current C++ agent) behaves like the serial row,")
    print("      and pipelining recovers most of the concurrent advantage.")


if __name__ == "__main__":
    sys.exit(main())
