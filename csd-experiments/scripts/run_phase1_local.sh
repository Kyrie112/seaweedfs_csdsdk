#!/usr/bin/env bash
# Phase-1 local experiment: deploy SeaweedFS (master + volume + filer) together
# with the software CSD agent and compare the three read paths.
#
# It answers the phase-1 questions without an FPGA:
#   - does a real needle payload get read through the aligned O_DIRECT window?
#   - do legacy / aligned / aligned+O_DIRECT produce identical results?
#   - how do they differ in latency, cold and warm?
#
# Usage:
#   WEED=/path/to/weed AGENT=/path/to/csd-agent-sim ./run_phase1_local.sh [size_mb] [reps]
set -u

WEED=${WEED:-/home/tan/new_workspace/tmpbuild/weed_phase1}
AGENT=${AGENT:-/tmp/csd-agent-sim}
WORK=${WORK:-/tmp/csd-exp}
CSV=${CSV:-$(cd "$(dirname "$0")/.." && pwd)/results/phase1_local.csv}
SIZE_MB=${1:-16}
REPS=${2:-5}
MODES=${MODES:-"legacy aligned direct auto"}

MASTER_PORT=19333
VOLUME_PORT=18080
FILER_PORT=18888
AGENT_PORT=18090
AGENT_URL="http://127.0.0.1:${AGENT_PORT}"

DATA_FILE="$WORK/data.bin"
UPLOAD_PATH="/data/phase1.bin"
COMPUTE_URL="http://127.0.0.1:${FILER_PORT}${UPLOAD_PATH}?compute=rawsum64"
# A plain (non-compute) read: this is what populates the page cache in a real
# deployment (FUSE mount, S3 GET, replication), and it is the situation the
# adaptive policy is meant to exploit.
PLAIN_URL="http://127.0.0.1:${FILER_PORT}${UPLOAD_PATH}"

AGENT_PID=""
MASTER_PID=""
VOLUME_PID=""
FILER_PID=""

cleanup() {
    [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null
    [ -n "$FILER_PID" ] && kill "$FILER_PID" 2>/dev/null
    [ -n "$VOLUME_PID" ] && kill "$VOLUME_PID" 2>/dev/null
    [ -n "$MASTER_PID" ] && kill "$MASTER_PID" 2>/dev/null
    wait 2>/dev/null
}
trap cleanup EXIT

log() { printf '[phase1] %s\n' "$*" >&2; }

wait_http() { # url timeout_s
    local url="$1" deadline=$((SECONDS + $2))
    while [ $SECONDS -lt $deadline ]; do
        if curl -s -o /dev/null "$url"; then return 0; fi
        sleep 0.3
    done
    return 1
}

start_agent() { # mode evict
    if [ -n "$AGENT_PID" ]; then
        kill "$AGENT_PID" 2>/dev/null
        wait "$AGENT_PID" 2>/dev/null
    fi
    "$AGENT" -listen="127.0.0.1:${AGENT_PORT}" -mode="$1" -evict="$2" \
        > "$WORK/agent_$1_$2.log" 2>&1 &
    AGENT_PID=$!
    wait_http "$AGENT_URL/v1/stats" 10 || { log "agent did not start"; exit 1; }
}

if [ ! -x "$WEED" ]; then log "weed binary not found: $WEED"; exit 2; fi
if [ ! -x "$AGENT" ]; then log "agent binary not found: $AGENT"; exit 2; fi

rm -rf "$WORK"
mkdir -p "$WORK/master" "$WORK/data" "$WORK/filer" "$WORK/scripts"
mkdir -p "$(dirname "$CSV")"

log "starting master"
"$WEED" master -mdir="$WORK/master" -port=$MASTER_PORT -ip=127.0.0.1 \
    -defaultReplication=000 -volumeSizeLimitMB=256 > "$WORK/master.log" 2>&1 &
MASTER_PID=$!
wait_http "http://127.0.0.1:${MASTER_PORT}/dir/status" 15 || { log "master failed"; cat "$WORK/master.log"; exit 1; }

log "starting volume server (compute + CSD enabled)"
"$WEED" volume -dir="$WORK/data" -port=$VOLUME_PORT -mserver="127.0.0.1:${MASTER_PORT}" \
    -ip=127.0.0.1 -max=4 -volume.compute.enabled=true \
    -volume.compute.dir="$WORK/scripts" \
    -volume.compute.csd.enabled=true \
    -volume.compute.csd.endpoint="$AGENT_URL" \
    > "$WORK/volume.log" 2>&1 &
VOLUME_PID=$!
wait_http "http://127.0.0.1:${VOLUME_PORT}/status" 15 || { log "volume failed"; cat "$WORK/volume.log"; exit 1; }

log "starting filer"
"$WEED" filer -master="127.0.0.1:${MASTER_PORT}" -port=$FILER_PORT -ip=127.0.0.1 \
    -defaultStoreDir="$WORK/filer" > "$WORK/filer.log" 2>&1 &
FILER_PID=$!
wait_http "http://127.0.0.1:${FILER_PORT}/" 20 || { log "filer failed"; cat "$WORK/filer.log"; exit 1; }

log "generating ${SIZE_MB}MB deterministic payload"
python3 - "$DATA_FILE" "$SIZE_MB" <<'GENEOF'
import sys
path, size_mb = sys.argv[1], int(sys.argv[2])
size = size_mb * 1024 * 1024
state = 0x2545F491
chunk = bytearray(1 << 20)
with open(path, "wb") as f:
    written = 0
    while written < size:
        for i in range(len(chunk)):
            state = (state * 1103515245 + 12345) & 0xFFFFFFFF
            chunk[i] = state >> 24
        take = min(len(chunk), size - written)
        f.write(chunk[:take])
        written += take
print(f"generated {size} bytes")
GENEOF

log "uploading to $UPLOAD_PATH"
curl -s -F "file=@$DATA_FILE" "http://127.0.0.1:${FILER_PORT}${UPLOAD_PATH}" > "$WORK/upload.json"
cat "$WORK/upload.json" >&2
echo >&2

echo "mode,cache,rep,http_code,total_ms,result,chunks,dispatch_ms,device_read_ms,kernel_ms,direct_hits,cache_hits,payload_bytes,agent_read_bytes,agent_direct" > "$CSV"

RESULT_FILE="$WORK/body.txt"
EXPECTED=""
LAST_CHUNKS=1

for mode in $MODES; do
  for cache in warm cold; do
    evict=false
    [ "$cache" = cold ] && evict=true
    log "=== mode=$mode cache=$cache ==="
    start_agent "$mode" "$evict"
    curl -s -o "$RESULT_FILE" "$COMPUTE_URL" > /dev/null   # untimed warm-up
    for rep in $(seq 1 "$REPS"); do
      if [ "$cache" = warm ]; then
        # Cache the payload through the normal read path, untimed.
        curl -s -o /dev/null "$PLAIN_URL"
      fi
      hdr="$WORK/hdr.txt"
      rm -f "$hdr"
      timing=$(curl -s -o "$RESULT_FILE" -D "$hdr" -w '%{http_code} %{time_total}' "$COMPUTE_URL")
      http_code=${timing%% *}
      total=${timing##* }
      result=$(cat "$RESULT_FILE")
      chunks=$(grep -i '^X-Compute-Chunks:' "$hdr" | tr -d '\r' | awk '{print $2}')
      dispatch=$(grep -i '^X-Compute-Dispatch-Ms:' "$hdr" | tr -d '\r' | awk '{print $2}')
      devread=$(grep -i '^X-Compute-Device-Read-Ms:' "$hdr" | tr -d '\r' | awk '{print $2}')
      kernel=$(grep -i '^X-Compute-Kernel-Ms:' "$hdr" | tr -d '\r' | awk '{print $2}')
      hits=$(grep -i '^X-Compute-Direct-Hits:' "$hdr" | tr -d '\r' | awk '{print $2}')
      chits=$(grep -i '^X-Compute-Cache-Hits:' "$hdr" | tr -d '\r' | awk '{print $2}')
      payload=$(grep -i '^X-Compute-Payload-Bytes:' "$hdr" | tr -d '\r' | awk '{print $2}')
      stats=$(curl -s "$AGENT_URL/v1/stats")
      area=$(printf '%s' "$stats" | python3 -c 'import json,sys; print(json.load(sys.stdin)["read_bytes"])')
      adirect=$(printf '%s' "$stats" | python3 -c 'import json,sys; print(json.load(sys.stdin)["direct_hits"])')
      total_ms=$(awk -v t="$total" 'BEGIN{printf "%.2f", t*1000}')
      printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$mode" "$cache" "$rep" "$http_code" "$total_ms" "$result" \
        "${chunks:-}" "${dispatch:-}" "${devread:-}" "${kernel:-}" "${hits:-}" "${chits:-0}" "${payload:-}" "$area" "$adirect" >> "$CSV"
      log "  rep=$rep ${total_ms}ms chunks=${chunks:-?} dispatch_sum=${dispatch:-?}ms read_sum=${devread:-?}ms kernel_sum=${kernel:-?}ms direct_hits=${hits:-?} cache_hits=${chits:-?} agent_read_bytes=$area"
      if [ -z "$EXPECTED" ]; then EXPECTED="$result"; fi
      LAST_CHUNKS="${chunks:-1}"
    done
  done
done

# Independent verification.
#
# Semantics: each chunk's result is the sum of little-endian uint64 words packed
# from that chunk's byte 0, wrapped to 64 bits; the filer then adds the per-chunk
# values with big.Int and deliberately does NOT wrap the aggregate. Chunk sizes
# are multiples of 8 bytes, so a chunk's packed sum is just the sum of the uint64
# words in its byte range.
log "independent verification against the original file"
python3 - "$DATA_FILE" "$EXPECTED" "$LAST_CHUNKS" "$WORK/verify.txt" <<'VERIFYEOF'
import sys
path, reported, chunk_count, out = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
MASK = (1 << 64) - 1
import os
size = os.path.getsize(path)
assert size % chunk_count == 0, f"{size} not divisible into {chunk_count} chunks"
chunk = size // chunk_count

total = 0
with open(path, "rb") as f:
    for _ in range(chunk_count):
        block = f.read(chunk)
        part = 0
        for i in range(0, len(block), 8):
            part = (part + int.from_bytes(block[i:i + 8], "little")) & MASK
        total += part          # aggregate is not wrapped, matching big.Int
status = "PASS" if str(total) == reported else "FAIL"
print(f"{status}: multi-chunk seaweedfs={reported} independent={total} "
      f"file={path} chunks={chunk_count} chunk_bytes={chunk}")
open(out, "w").write(status)
VERIFYEOF

# Second, assumption-free check: a file small enough to live in a single chunk
# must round-trip exactly, with no chunk-boundary reasoning involved.
log "single-chunk verification (2MB file)"
SINGLE_FILE="$WORK/single.bin"
head -c 2097152 "$DATA_FILE" > "$SINGLE_FILE"
curl -s -F "file=@$SINGLE_FILE" "http://127.0.0.1:${FILER_PORT}/data/single.bin" > /dev/null
start_agent direct false
curl -s -o "$WORK/single_body.txt" -D "$WORK/single_hdr.txt" \
    "http://127.0.0.1:${FILER_PORT}/data/single.bin?compute=rawsum64"
SINGLE_RESULT=$(cat "$WORK/single_body.txt")
SINGLE_CHUNKS=$(grep -i '^X-Compute-Chunks:' "$WORK/single_hdr.txt" | tr -d '\r' | awk '{print $2}')
SINGLE_HITS=$(grep -i '^X-Compute-Direct-Hits:' "$WORK/single_hdr.txt" | tr -d '\r' | awk '{print $2}')
python3 - "$SINGLE_FILE" "$SINGLE_RESULT" "$SINGLE_CHUNKS" "$SINGLE_HITS" "$WORK/verify_single.txt" <<'SINGLEEOF'
import sys, os
path, reported, chunks, hits, out = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
MASK = (1 << 64) - 1
total = 0
with open(path, "rb") as f:
    block = f.read()
for i in range(0, len(block), 8):
    total = (total + int.from_bytes(block[i:i + 8], "little")) & MASK
ok = str(total) == reported
status = "PASS" if ok else "FAIL"
print(f"{status}: single-chunk seaweedfs={reported} independent={total} "
      f"chunks={chunks} direct_hits={hits} file={path}")
open(out, "w").write(status)
SINGLEEOF

log "summary (median, min, max per mode/cache)"
python3 - "$CSV" <<'SUMMARYEOF'
import csv, statistics, sys
rows = list(csv.DictReader(open(sys.argv[1])))
buckets = {}
for r in rows:
    buckets.setdefault((r["mode"], r["cache"]), []).append(float(r["total_ms"]))
order = ["legacy", "aligned", "direct", "auto"]
order = [m for m in order if m in {k[0] for k in buckets}]
print(f"{'mode':8} {'cache':5} {'n':>3} {'median_ms':>10} {'min_ms':>8} {'max_ms':>8}")
for key in sorted(buckets, key=lambda k: (order.index(k[0]), k[1])):
    vals = buckets[key]
    print(f"{key[0]:8} {key[1]:5} {len(vals):>3} {statistics.median(vals):>10.2f} {min(vals):>8.2f} {max(vals):>8.2f}")
SUMMARYEOF

log "CSV written to $CSV"
