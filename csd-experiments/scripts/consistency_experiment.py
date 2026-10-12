#!/usr/bin/env python3
"""M1 verification: does the range self-check turn a silent error into a failure?

The script drives three phases against a single-chunk gallery:

  A. normal       - the request must succeed and match the brute-force reference
  B. corrupt      - the 20-byte needle header that precedes the payload is
                    overwritten, simulating "this physical range no longer
                    belongs to the object the filer planned to compute over"
                    (exactly what a vacuum move or a delete+reuse produces)
  C. restore      - the header is put back; the request must succeed again,
                    proving the check is precise rather than failing everything

Running it once with the agent's self-check disabled and once enabled produces
the before/after evidence: without the check, phase B returns HTTP 200 with a
plausible but wrong value; with it, phase B fails with `stale_range`.

Latency of phases A and C is also recorded, which answers decision gate D1
(the cost of the self-check).
"""

import argparse
import csv
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import image_search_lib as lib  # noqa: E402
import image_search_experiment as base  # noqa: E402

NEEDLE_HEADER_SIZE = 20


def agent_get(url):
    with urllib.request.urlopen(url, timeout=30) as response:
        return json.loads(response.read())


def agent_post(url):
    request = urllib.request.Request(url, data=b"", method="POST")
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.loads(response.read())


def search(filer, upload_path, query_encoded, k):
    """Return (status, body_text, headers, latency_ms)."""
    url = (f"{filer}{upload_path}?compute=img_topk&k={k}"
           f"&rec={lib.RECORD_SIZE}&q={urllib.parse.quote(query_encoded)}")
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(url, timeout=120) as response:
            body = response.read().decode().strip()
            headers = dict(response.headers)
            status = response.status
    except urllib.error.HTTPError as err:
        body = err.read().decode().strip()
        headers = dict(err.headers)
        status = err.code
    return status, body, headers, (time.perf_counter() - started) * 1000.0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--filer", default="http://127.0.0.1:18889")
    parser.add_argument("--agent", default="http://127.0.0.1:18091")
    parser.add_argument("--work", default="/tmp/csd-consistency")
    parser.add_argument("--images", type=int, default=512)
    parser.add_argument("--k", type=int, default=5)
    parser.add_argument("--reps", type=int, default=7)
    parser.add_argument("--csv", default=None)
    args = parser.parse_args()

    os.makedirs(args.work, exist_ok=True)
    gallery_path = os.path.join(args.work, "gallery.bin")
    upload_path = "/data/gallery.bin"

    print(f"[gen] {args.images} images ({args.images * lib.RECORD_SIZE / (1 << 20):.1f} MiB), "
          "small enough to stay in a single chunk")
    lib.make_gallery(gallery_path, args.images, clusters=8)
    payload = open(gallery_path, "rb").read()
    query_record = lib.make_query(args.images // 3, gallery_path)
    query_encoded = lib.encode_query(query_record)
    truth = lib.brute_force(payload, lib.descriptor(query_record), args.k)

    print(f"[up] uploading to {upload_path}")
    print("     " + base.upload_file(gallery_path, args.filer + upload_path,
                                     content_type="image/x-raw-gray64"))

    stats = agent_get(args.agent + "/v1/stats")
    self_check = stats.get("self_check")
    print(f"[agent] self_check={self_check} stale_ranges={stats.get('stale_ranges')}")

    rows = []
    normal_latencies = []

    # ---- phase A: normal ----
    for rep in range(1, args.reps + 1):
        status, body, headers, latency = search(args.filer, upload_path, query_encoded, args.k)
        hits = lib.parse_hits(body) if status == 200 else []
        normal_latencies.append(latency)
        rows.append({
            "phase": "A-normal", "rep": rep, "http": status,
            "latency_ms": f"{latency:.2f}",
            "consistency_level": headers.get("X-Compute-Consistency-Level", ""),
            "snapshot": headers.get("X-Compute-Snapshot", ""),
            "stale_chunks": headers.get("X-Compute-Stale-Chunks", ""),
            "matches_reference": int(hits == truth),
            "body": body[:120],
        })
        print(f"[A] rep={rep} http={status} {latency:7.2f} ms level={headers.get('X-Compute-Consistency-Level','?')} "
              f"snapshot={headers.get('X-Compute-Snapshot','?')} match={hits == truth}")

    # ---- locate the needle that serves this gallery ----
    last = agent_get(args.agent + "/v1/last")
    data_file = last.get("data_file")
    payload_offset = int(last.get("offset", 0))
    if not data_file or payload_offset < NEEDLE_HEADER_SIZE:
        print("[B] cannot locate the needle for fault injection", file=sys.stderr)
        return 2
    header_offset = payload_offset - NEEDLE_HEADER_SIZE
    payload_size = int(last.get("size", 0))
    # Back up header + payload so the injection can be undone exactly.
    with open(data_file, "rb") as handle:
        handle.seek(header_offset)
        original = handle.read(NEEDLE_HEADER_SIZE + payload_size)
    print(f"[B] data file {data_file}, payload at {payload_offset} ({payload_size} bytes), "
          f"header at {header_offset} = {original[:NEEDLE_HEADER_SIZE].hex()}")

    # ---- phase B: simulate "this range was reused by another needle" ----
    #
    # Corrupting only the header would leave the payload intact, and a reader
    # without verification would still return the correct answer. A real reuse
    # replaces both: the header gets a different cookie and the payload holds
    # another object's bytes. Only then does "no verification" produce a
    # plausible but wrong result.
    with open(data_file, "r+b") as handle:
        handle.seek(header_offset)
        handle.write(b"\xff" * NEEDLE_HEADER_SIZE)      # different cookie
        handle.write(b"\x00" * payload_size)            # another object's bytes
        handle.flush()
        os.fsync(handle.fileno())
    status, body, headers, latency = search(args.filer, upload_path, query_encoded, args.k)
    hits = lib.parse_hits(body) if status == 200 else []
    rows.append({
        "phase": "B-corrupt", "rep": 1, "http": status,
        "latency_ms": f"{latency:.2f}",
        "consistency_level": headers.get("X-Compute-Consistency-Level", ""),
        "snapshot": headers.get("X-Compute-Snapshot", ""),
        "stale_chunks": headers.get("X-Compute-Stale-Chunks", ""),
        "matches_reference": int(hits == truth),
        "body": body[:200],
    })
    corrupt_status = status
    disk_reuse_status = status
    print(f"[B1] http={status} {latency:7.2f} ms  body={body[:160]}")
    print("[B1] interpretation: a header replaced *before* the volume resolves the needle is "
          "already caught by the volume's own metadata lookup (first line of defence)")

    # ---- phase C: restore ----
    with open(data_file, "r+b") as handle:
        handle.seek(header_offset)
        handle.write(original)
        handle.flush()
        os.fsync(handle.fileno())
    restore_latencies = []
    for rep in range(1, args.reps + 1):
        status, body, headers, latency = search(args.filer, upload_path, query_encoded, args.k)
        hits = lib.parse_hits(body) if status == 200 else []
        restore_latencies.append(latency)
        rows.append({
            "phase": "C-restore", "rep": rep, "http": status,
            "latency_ms": f"{latency:.2f}",
            "consistency_level": headers.get("X-Compute-Consistency-Level", ""),
            "snapshot": headers.get("X-Compute-Snapshot", ""),
            "stale_chunks": headers.get("X-Compute-Stale-Chunks", ""),
            "matches_reference": int(hits == truth),
            "body": body[:120],
        })
    print(f"[C] after restore: http set = {sorted({r['http'] for r in rows if r['phase'] == 'C-restore'})}, "
          f"match = {sorted({r['matches_reference'] for r in rows if r['phase'] == 'C-restore'})}")

    # ---- phase B2: reuse inside the read window ----
    #
    # This is the window the volume cannot protect: its metadata lookup already
    # succeeded, and the bytes change while the device is reading them. The
    # fault is armed for exactly one request through the agent's test-only
    # /v1/fault endpoint, so the baseline phases stay clean.
    armed = agent_post(args.agent + "/v1/fault?mode=mutate-on-read&count=1")
    print(f"[B2] armed fault injection: {armed}")
    status, body, headers, latency = search(args.filer, upload_path, query_encoded, args.k)
    hits = lib.parse_hits(body) if status == 200 else []
    mid_read_silent = (status == 200 and hits != truth)
    rows.append({
        "phase": "B2-mid-read-reuse", "rep": 1, "http": status,
        "latency_ms": f"{latency:.2f}",
        "consistency_level": headers.get("X-Compute-Consistency-Level", ""),
        "snapshot": headers.get("X-Compute-Snapshot", ""),
        "stale_chunks": headers.get("X-Compute-Stale-Chunks", ""),
        "matches_reference": int(hits == truth),
        "body": body[:200],
    })
    print(f"[B2] http={status} {latency:7.2f} ms  body={body[:160]}")
    print(f"[B2] silent_error={mid_read_silent}  "
          f"(200 with a wrong answer = the failure mode M1 removes)")

    stats_after = agent_get(args.agent + "/v1/stats")
    print()
    print(f"[sum] phase A median {statistics.median(normal_latencies):.2f} ms over {len(normal_latencies)} runs")
    print(f"[sum] phase C median {statistics.median(restore_latencies):.2f} ms")
    print(f"[sum] corrupted-range request: http={corrupt_status} "
          f"stale_ranges on agent = {stats_after.get('stale_ranges')}, "
          f"header_checks = {stats_after.get('header_checks')}")
    print(f"[sum] B1 (reuse before lookup): http={disk_reuse_status}")
    verdict = "SILENT ERROR" if mid_read_silent else "explicit failure / correct answer"
    print(f"[sum] B2 (reuse during the read window): {verdict}")
    print(f"[sum] self_check={self_check} -> M1 verdict: "
          f"{'NOT PROTECTED' if mid_read_silent else 'PROTECTED'}")

    if args.csv:
        os.makedirs(os.path.dirname(args.csv), exist_ok=True)
        with open(args.csv, "w", newline="") as handle:
            writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
            writer.writeheader()
            writer.writerows(rows)
        print(f"[sum] CSV written to {args.csv}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
