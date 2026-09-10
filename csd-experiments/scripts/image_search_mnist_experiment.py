#!/usr/bin/env python3
"""Near-storage image similarity search on a real image dataset (MNIST).

The synthetic gallery in image_search_experiment.py proves the mechanics, but
its clusters are artificial. This variant runs the same operator over the MNIST
test set (real handwritten digits with ground-truth labels), which supports two
claims that a synthetic gallery cannot:

  * the near-storage answer still equals a brute-force reference, and
  * retrieval quality on real images, measured as precision@K against the digit
    label and as the same-class / different-class distance gap.

Queries are taken from the part of the test set that is NOT part of the gallery,
so the query image is genuinely unseen by the indexed data.
"""

import argparse
import csv
import gzip
import os
import statistics
import struct
import sys
import time
import urllib.parse

from PIL import Image

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import image_search_lib as lib  # noqa: E402
import image_search_experiment as base  # noqa: E402


def load_idx(path):
    """Read an IDX file (the MNIST container format) without numpy."""
    with gzip.open(path, "rb") as handle:
        magic = struct.unpack(">I", handle.read(4))[0]
        dims = magic & 0xFF
        sizes = struct.unpack(">" + "I" * dims, handle.read(4 * dims))
        payload = handle.read()
    expected = 1
    for size in sizes:
        expected *= size
    if len(payload) != expected:
        raise ValueError(f"{path}: expected {expected} values, got {len(payload)}")
    return sizes, payload


def load_mnist(data_dir):
    sizes, images = load_idx(os.path.join(data_dir, "t10k-images-idx3-ubyte.gz"))
    _, labels = load_idx(os.path.join(data_dir, "t10k-labels-idx1-ubyte.gz"))
    count, height, width = sizes
    return images, labels, count, height, width


def to_record(raw, height, width):
    """Upscale a 28x28 digit to the 64x64 grayscale record the operator uses."""
    image = Image.frombytes("L", (width, height), raw).resize((64, 64), Image.NEAREST)
    return image.tobytes()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--filer", default="http://127.0.0.1:18889")
    parser.add_argument("--work", default="/tmp/csd-mnist")
    parser.add_argument("--data-dir", default="/home/tan/new_workspace/tmpbuild/datasets/mnist")
    parser.add_argument("--images", type=int, default=4096, help="gallery size")
    parser.add_argument("--k", type=int, default=5)
    parser.add_argument("--reps", type=int, default=5)
    parser.add_argument("--query-offset", type=int, default=0,
                        help="which held-out image to use as the timed query")
    parser.add_argument("--precision-queries", type=int, default=30,
                        help="how many held-out queries to average precision@K over")
    parser.add_argument("--csv", default=None)
    args = parser.parse_args()

    os.makedirs(args.work, exist_ok=True)
    gallery_path = os.path.join(args.work, "mnist_gallery.bin")
    upload_path = "/data/mnist.bin"
    compute_base = args.filer + upload_path + "?compute=img_topk"

    print("[data] loading MNIST test set")
    images, labels, count, height, width = load_mnist(args.data_dir)
    if args.images + 1 > count:
        print(f"gallery of {args.images} + query exceeds the {count} available images",
              file=sys.stderr)
        return 2
    gallery_labels = [labels[i] for i in range(args.images)]

    with open(gallery_path, "wb") as handle:
        for index in range(args.images):
            start = index * height * width
            handle.write(to_record(images[start:start + height * width], height, width))
    payload = open(gallery_path, "rb").read()
    print(f"[data] gallery: {args.images} real digits, "
          f"{len(payload) / (1 << 20):.1f} MiB ({len(payload)} bytes)")

    query_index = args.images + args.query_offset
    query_label = labels[query_index]
    query_start = query_index * height * width
    query_record = to_record(images[query_start:query_start + height * width], height, width)
    query_descriptor = lib.descriptor(query_record)
    query_encoded = lib.encode_query(query_record)

    # Descriptors of the gallery are computed once; every later comparison
    # (ground truth, separation statistics, multi-query precision) reuses them.
    gallery_descriptors = [
        lib.descriptor(payload[index * lib.RECORD_SIZE:(index + 1) * lib.RECORD_SIZE])
        for index in range(args.images)
    ]

    def rank(descriptor):
        scored = [(index, lib.distance(descriptor, gallery_descriptors[index]))
                  for index in range(args.images)]
        return sorted(scored, key=lambda hit: (hit[1], hit[0]))

    scored = rank(query_descriptor)
    truth = scored[:args.k]
    print(f"[ref] query = held-out image #{query_index} (digit {query_label}); "
          f"brute-force top-{args.k}: {truth}")

    print(f"[up] uploading to {upload_path}")
    print("     " + base.upload_file(gallery_path, args.filer + upload_path,
                                     content_type="image/x-raw-gray64"))

    rows = []
    latencies = []
    for rep in range(1, args.reps + 1):
        url = (f"{compute_base}&k={args.k}&rec={lib.RECORD_SIZE}"
               f"&q={urllib.parse.quote(query_encoded)}")
        body, headers, elapsed_ms = base.http_get(url)
        text = body.decode().strip()
        hits = lib.parse_hits(text)
        latency = elapsed_ms
        latencies.append(latency)
        same_class = sum(1 for index, _ in hits if gallery_labels[index] == query_label)
        rows.append({
            "phase": "near-storage", "rep": rep, "k": args.k,
            "latency_ms": f"{latency:.2f}", "bytes_returned": len(body),
            "chunks": headers.get("X-Compute-Chunks", ""),
            "device_read_ms": headers.get("X-Compute-Device-Read-Ms", ""),
            "exact": int(hits == truth), "same_class": same_class,
            "hits": text,
        })
        print(f"[run] rep={rep} {latency:8.2f} ms bytes={len(body):4d} "
              f"chunks={headers.get('X-Compute-Chunks', '?')} "
              f"exact={hits == truth} same_class={same_class}/{args.k}  {text}")

    # Distance statistics: how well does the descriptor separate digit classes?
    same = [d for index, d in scored if gallery_labels[index] == query_label]
    other = [d for index, d in scored if gallery_labels[index] != query_label]
    print(f"[sep] same-digit distance: median {statistics.median(same):.0f} "
          f"(n={len(same)}); other digits: median {statistics.median(other):.0f}")

    raw_body, _, download_ms = base.http_get(args.filer + upload_path)
    started = time.perf_counter()
    local_hits = lib.brute_force(raw_body, query_descriptor, args.k)
    compute_ms = (time.perf_counter() - started) * 1000.0
    baseline_ms = download_ms + compute_ms
    print(f"[base] download {len(raw_body)} bytes in {download_ms:.2f} ms, "
          f"client search {compute_ms:.2f} ms, total {baseline_ms:.2f} ms")
    rows.append({
        "phase": "download+client-search", "rep": 1, "k": args.k,
        "latency_ms": f"{baseline_ms:.2f}", "bytes_returned": len(raw_body),
        "chunks": "", "device_read_ms": f"{download_ms:.2f}",
        "exact": int(local_hits == truth),
        "same_class": sum(1 for index, _ in local_hits if gallery_labels[index] == query_label),
        "hits": ",".join(f"{i}:{d}" for i, d in local_hits),
    })

    returned = int(rows[0]["bytes_returned"])
    exact_runs = sum(1 for r in rows if r["phase"] == "near-storage" and r["exact"] == 1)
    precision = [r["same_class"] for r in rows if r["phase"] == "near-storage"]

    print()
    print(f"[sum] near-storage median {statistics.median(latencies):.2f} ms, "
          f"returned {returned} bytes")
    print(f"[sum] baseline {baseline_ms:.2f} ms, transferred {len(raw_body)} bytes")
    print(f"[sum] traffic reduction: {100 * (1 - returned / len(raw_body)):.6f}% "
          f"({len(raw_body) / max(returned, 1):.0f}x smaller response)")
    print(f"[sum] exactness: {exact_runs}/{len(latencies)} runs equal to brute force")
    print(f"[sum] precision@{args.k} on digit labels (single query): "
          f"{statistics.mean(precision):.2f} (runs: {precision})")

    # Average precision@K over several *held-out* queries: a single query is a
    # weak estimator of retrieval quality.
    multi = []
    for offset in range(args.precision_queries):
        index = args.images + offset
        if index >= count:
            break
        start = index * height * width
        descriptor = lib.descriptor(to_record(images[start:start + height * width], height, width))
        top = [i for i, _ in rank(descriptor)[:args.k]]
        multi.append(sum(1 for i in top if gallery_labels[i] == labels[index]) / args.k)
    if multi:
        print(f"[sum] precision@{args.k} averaged over {len(multi)} held-out queries: "
              f"{statistics.mean(multi):.3f}")
        rows.append({
            "phase": f"precision@{args.k}-{len(multi)}-queries", "rep": 1, "k": args.k,
            "latency_ms": "", "bytes_returned": "", "chunks": "", "device_read_ms": "",
            "exact": "", "same_class": f"{statistics.mean(multi):.3f}", "hits": "",
        })

    if args.csv:
        os.makedirs(os.path.dirname(args.csv), exist_ok=True)
        with open(args.csv, "w", newline="") as handle:
            writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
            writer.writeheader()
            writer.writerows(rows)
        print(f"[sum] CSV written to {args.csv}")

    if exact_runs != len(latencies):
        print("[sum] FAIL: some runs disagree with the brute-force reference",
              file=sys.stderr)
        return 1
    print("[sum] PASS: every run matches the brute-force reference")
    return 0


if __name__ == "__main__":
    sys.exit(main())
