#!/usr/bin/env python3
"""M4 fault-injection matrix and invariant checker.

Every row runs a fault, captures what the request returned, and applies the
machine-checkable rule from the plan:

    PASS := the request failed explicitly
         or the answer equals the computation over some single snapshot
    FAIL := the request succeeded but the answer belongs to no snapshot
            (a silent error - the class of bug this whole effort removes)

Rows that need an environment this container cannot provide (three reachable
hosts, a rolling upgrade, a live volume server) are recorded as SKIPPED with the
reason, rather than being silently omitted.
"""

import argparse
import csv
import json
import os
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import image_search_lib as lib  # noqa: E402
import image_search_experiment as base  # noqa: E402


def get(url, timeout=120):
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return response.status, response.read().decode().strip(), dict(response.headers), \
                (time.perf_counter() - started) * 1000.0
    except urllib.error.HTTPError as err:
        return err.code, err.read().decode().strip(), dict(err.headers), \
            (time.perf_counter() - started) * 1000.0


def post(url, timeout=60):
    request = urllib.request.Request(url, data=b"", method="POST")
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read())


def judge(status, hits, snapshots):
    """Apply the invariant: the answer must belong to some single snapshot."""
    if status != 200:
        return "PASS", f"explicit failure (http {status})"
    for name, truth in snapshots.items():
        if hits == truth:
            return "PASS", f"matches snapshot {name}"
    return "FAIL", "returned a result that matches no snapshot (silent error)"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--filer", default="http://127.0.0.1:19600")
    parser.add_argument("--agent", default="http://127.0.0.1:19800")
    parser.add_argument("--work", default="/home/tan/new_workspace/tmpbuild/multinode")
    parser.add_argument("--images", type=int, default=1024)
    parser.add_argument("--k", type=int, default=5)
    parser.add_argument("--csv", default=None)
    parser.add_argument("--volume-pids", default="",
                        help="comma separated volume server pids for the node-failure row")
    args = parser.parse_args()

    os.makedirs(args.work, exist_ok=True)
    gallery_path = os.path.join(args.work, "matrix_gallery.bin")
    upload_path = "/data/matrix.bin"
    if not os.path.exists(gallery_path):
        lib.make_gallery(gallery_path, args.images, clusters=8)
    payload = open(gallery_path, "rb").read()
    query_record = lib.make_query(args.images // 3, gallery_path)
    query_encoded = lib.encode_query(query_record)
    query_descriptor = lib.descriptor(query_record)
    truth_a = lib.brute_force(payload, query_descriptor, args.k)
    snapshots = {"A": truth_a}

    print(f"[matrix] uploading {len(payload)} bytes to {upload_path}")
    base.upload_file(gallery_path, args.filer + upload_path, content_type="image/x-raw-gray64")

    url_base = (f"{args.filer}{upload_path}?compute=img_topk&k={args.k}"
                f"&rec={lib.RECORD_SIZE}&q={urllib.parse.quote(query_encoded)}")

    rows = []

    def record(row_id, scenario, expect, status, body, headers, latency, verdict, note):
        rows.append({
            "id": row_id, "scenario": scenario, "expectation": expect,
            "http": status, "latency_ms": f"{latency:.2f}",
            "verdict": verdict, "note": note,
            "error_class": headers.get("X-Compute-Error-Class", ""),
            "retry_reason": headers.get("X-Compute-Retry-Reason", ""),
            "body": body[:120],
        })
        print(f"[{row_id}] {scenario}: http={status} verdict={verdict} ({note})")

    # ---- baseline ----
    status, body, headers, latency = get(url_base)
    hits = lib.parse_hits(body) if status == 200 else []
    verdict, note = judge(status, hits, snapshots)
    record("baseline", "no fault", "correct answer", status, body, headers, latency, verdict, note)

    # ---- F4: the range is reused while it is being read ----
    post(args.agent + "/v1/fault?mode=mutate-on-read&count=1")
    status, body, headers, latency = get(url_base)
    hits = lib.parse_hits(body) if status == 200 else []
    verdict, note = judge(status, hits, snapshots)
    if status == 200 and verdict == "PASS":
        note += " (recovered by the M2 retry)"
    record("F4", "range reused during the read", "explicit failure or a snapshot answer",
           status, body, headers, latency, verdict, note)

    # ---- F1: the file is overwritten around the request ----
    # Uploading a different gallery and issuing a request immediately exercises
    # the "metadata changed while planning" window. The answer must correspond to
    # one of the two versions, never to a mix.
    other_path = os.path.join(args.work, "matrix_gallery_v2.bin")
    lib.make_gallery(other_path, args.images, clusters=8, seed=777)
    other_payload = open(other_path, "rb").read()
    snapshots["B"] = lib.brute_force(other_payload, query_descriptor, args.k)
    base.upload_file(other_path, args.filer + upload_path, content_type="image/x-raw-gray64")
    status, body, headers, latency = get(url_base)
    hits = lib.parse_hits(body) if status == 200 else []
    verdict, note = judge(status, hits, snapshots)
    record("F1", "file overwritten around the request", "answer belongs to snapshot A or B",
           status, body, headers, latency, verdict, note)

    # ---- F7: a replica is gone ----
    pids = [p for p in args.volume_pids.split(",") if p.strip()]
    if pids:
        victim = int(pids[0])
        try:
            os.kill(victim, 15)
            print(f"[F7] stopped volume server pid {victim}")
            time.sleep(2)
        except ProcessLookupError:
            print(f"[F7] pid {victim} already gone")
        status, body, headers, latency = get(url_base)
        hits = lib.parse_hits(body) if status == 200 else []
        verdict, note = judge(status, hits, snapshots)
        if status == 200:
            note += " (served by another replica)"
        record("F7", "one replica unavailable", "still served, or explicit failure",
               status, body, headers, latency, verdict, note)
    else:
        record("F7", "one replica unavailable", "still served, or explicit failure",
               "", "", {}, 0, "SKIPPED", "no volume server pids given")

    # ---- rows that need an environment this container cannot provide ----
    for row_id, scenario, why in [
        ("F2", "file deleted while computing", "needs a stable multi-host cluster to time the delete window"),
        ("F3", "compaction during compute", "needs a volume with reclaimable garbage plus a vacuum trigger; covered by the storage-level integration test"),
        ("F5", "one replica lags behind", "needs the three-host deployment to control replica progress"),
        ("F6", "mixed kernel/protocol versions", "needs a rolling upgrade across hosts"),
    ]:
        record(row_id, scenario, "explicit failure or a snapshot answer", "", "", {}, 0,
               "SKIPPED", why)

    if args.csv:
        os.makedirs(os.path.dirname(args.csv), exist_ok=True)
        with open(args.csv, "w", newline="") as handle:
            writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
            writer.writeheader()
            writer.writerows(rows)
        print(f"[matrix] CSV written to {args.csv}")

    failed = [row for row in rows if row["verdict"] == "FAIL"]
    skipped = [row for row in rows if row["verdict"] == "SKIPPED"]
    print()
    print(f"[sum] rows: {len(rows)}, silent errors: {len(failed)}, skipped: {len(skipped)}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
