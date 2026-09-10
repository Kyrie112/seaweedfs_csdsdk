#!/usr/bin/env python3
"""Near-storage image similarity search experiment.

Requires a running SeaweedFS cluster (master + volume with CSD enabled) and a
CSD agent. Generates a synthetic gallery of clustered thumbnails, uploads it as
one gallery file, asks the cluster for the top-k most similar images with a
query descriptor, and compares:

  - the near-storage answer (device scans the data, only k hits travel back)
  - a brute-force reference over the same bytes (ground truth)
  - a "traditional" baseline that downloads the whole gallery and searches
    client-side
"""

import argparse
import csv
import json
import os
import statistics
import sys
import time
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import image_search_lib as lib  # noqa: E402


def http_get(url, timeout=600):
    started = time.perf_counter()
    with urllib.request.urlopen(url, timeout=timeout) as response:
        body = response.read()
        headers = dict(response.headers)
    return body, headers, (time.perf_counter() - started) * 1000.0


def upload_file(path, url, content_type="application/octet-stream"):
    """Upload a file to the filer.

    The content type matters: SeaweedFS decides whether to gzip a needle from
    the MIME type and from a 128-byte compression sample. Raw image payloads
    must be stored uncompressed, otherwise the needle is compressed and the CSD
    path rejects it (compressed needles are not yet supported by the device
    operator). Declaring an image/* type tells the storage layer that the data
    is already an image format and must not be gzipped.
    """
    boundary = "----csdimgsearchboundary"
    with open(path, "rb") as handle:
        payload = handle.read()
    body = b"".join([
        ("--%s\r\n" % boundary).encode(),
        ('Content-Disposition: form-data; name="file"; filename="%s"\r\n'
         % os.path.basename(path)).encode(),
        ("Content-Type: %s\r\n\r\n" % content_type).encode(),
        payload,
        ("\r\n--%s--\r\n" % boundary).encode(),
    ])
    request = urllib.request.Request(
        url, data=body, method="POST",
        headers={"Content-Type": "multipart/form-data; boundary=%s" % boundary})
    with urllib.request.urlopen(request, timeout=600) as response:
        return response.read().decode()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--filer", default="http://127.0.0.1:18889")
    parser.add_argument("--work", default="/tmp/csd-imgsearch")
    parser.add_argument("--images", type=int, default=4096)
    parser.add_argument("--clusters", type=int, default=8)
    parser.add_argument("--k", type=int, default=5)
    parser.add_argument("--reps", type=int, default=7)
    parser.add_argument("--query-index", type=int, default=1234)
    parser.add_argument("--csv", default=None)
    args = parser.parse_args()

    os.makedirs(args.work, exist_ok=True)
    gallery_path = os.path.join(args.work, "gallery.bin")
    upload_path = "/data/gallery.bin"
    compute_url_base = args.filer + upload_path + "?compute=img_topk"

    print(f"[gen] {args.images} images x {lib.RECORD_SIZE}B "
          f"({args.images * lib.RECORD_SIZE / (1 << 20):.1f} MiB), {args.clusters} clusters")
    labels = lib.make_gallery(gallery_path, args.images, clusters=args.clusters)
    payload = open(gallery_path, "rb").read()

    query_index = args.query_index % args.images if args.images else 0
    query_record = lib.make_query(query_index, gallery_path)
    query_descriptor = lib.descriptor(query_record)
    query_encoded = lib.encode_query(query_record)
    truth = lib.brute_force(payload, query_descriptor, args.k)
    query_cluster = labels[query_index]
    print(f"[ref] query = image #{query_index} (cluster {query_cluster}), "
          f"ground-truth top-{args.k}: {truth}")

    print(f"[up] uploading to {upload_path}")
    print("     " + upload_file(gallery_path, args.filer + upload_path))

    rows = []
    latencies = []
    response_bytes = 0
    for rep in range(1, args.reps + 1):
        url = (f"{compute_url_base}&k={args.k}&rec={lib.RECORD_SIZE}"
               f"&q={urllib.parse.quote(query_encoded)}")
        body, headers, elapsed_ms = http_get(url)
        text = body.decode().strip()
        hits = lib.parse_hits(text)
        match = hits == truth
        latencies.append(elapsed_ms)
        response_bytes = len(body)
        rows.append({
            "phase": "near-storage", "rep": rep, "k": args.k,
            "latency_ms": f"{elapsed_ms:.2f}", "bytes_returned": len(body),
            "chunks": headers.get("X-Compute-Chunks", ""),
            "dispatch_ms": headers.get("X-Compute-Dispatch-Ms", ""),
            "device_read_ms": headers.get("X-Compute-Device-Read-Ms", ""),
            "kernel_ms": headers.get("X-Compute-Kernel-Ms", ""),
            "matches_reference": int(match), "hits": text,
        })
        print(f"[run] rep={rep} {elapsed_ms:8.2f} ms  bytes={len(body):5d} "
              f"chunks={headers.get('X-Compute-Chunks', '?')} "
              f"match={match}  {text}")
        if not match:
            print(f"[run] MISMATCH: got {hits} want {truth}", file=sys.stderr)

    # Traditional baseline: fetch the whole gallery, search on the client.
    raw_body, raw_headers, download_ms = http_get(args.filer + upload_path)
    started = time.perf_counter()
    local_hits = lib.brute_force(raw_body, query_descriptor, args.k)
    compute_ms = (time.perf_counter() - started) * 1000.0
    baseline_ms = download_ms + compute_ms
    print(f"[base] download {len(raw_body)} bytes in {download_ms:.2f} ms, "
          f"client search {compute_ms:.2f} ms, total {baseline_ms:.2f} ms")
    print(f"[base] client top-{args.k}: {local_hits} (matches reference: {local_hits == truth})")
    rows.append({
        "phase": "download+client-search", "rep": 1, "k": args.k,
        "latency_ms": f"{baseline_ms:.2f}", "bytes_returned": len(raw_body),
        "chunks": "", "dispatch_ms": f"{download_ms:.2f}",
        "device_read_ms": "", "kernel_ms": f"{compute_ms:.2f}",
        "matches_reference": int(local_hits == truth),
        "hits": ",".join(f"{i}:{d}" for i, d in local_hits),
    })

    print()
    print("[sum] near-storage median %.2f ms, returned %d bytes"
          % (statistics.median(latencies), response_bytes))
    print("[sum] baseline median %.2f ms, transferred %d bytes"
          % (baseline_ms, len(raw_body)))
    print("[sum] data traffic reduction: %.6f%% (%.1fx smaller response)"
          % (100.0 * (1 - response_bytes / len(raw_body)),
             len(raw_body) / max(response_bytes, 1)))

    top_hits = [index for index, _ in truth[:args.k]]
    same_cluster = sum(1 for index in top_hits if labels[index] == query_cluster)
    print("[sum] top-%d precision w.r.t. the query cluster: %d/%d"
          % (args.k, same_cluster, len(top_hits)))

    if args.csv:
        os.makedirs(os.path.dirname(args.csv), exist_ok=True)
        with open(args.csv, "w", newline="") as handle:
            writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
            writer.writeheader()
            writer.writerows(rows)
        print(f"[sum] CSV written to {args.csv}")

    if any(row["matches_reference"] != 1 for row in rows):
        print("[sum] FAIL: some runs disagree with the brute-force reference",
              file=sys.stderr)
        return 1
    print("[sum] PASS: every run matches the brute-force reference")
    return 0


if __name__ == "__main__":
    sys.exit(main())
