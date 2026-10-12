#!/usr/bin/env bash
# Local multi-volume-server deployment used to verify M3 (version retention and
# reclamation) and the replica-related parts of M1/M2.
#
# Topology (all on this machine, distinct ports):
#   master x1, volume server x3 (each with its own CSD agent), filer x1
#
# It then:
#   1. uploads a gallery with one replica on top of the primary (replication 001)
#      so chunks have more than one candidate replica;
#   2. runs compute requests (which retain a data version on the serving volume);
#   3. queries GET /compute/versions on every volume server to show which data
#      versions are retained and when they were last used;
#   4. forces a collection pass with a short retention and shows the old version
#      disappearing while the current one stays.
#
# Usage: WEED=... AGENT=... ./run_local_multinode.sh [images] [k]
set -u

WEED=${WEED:-/home/tan/new_workspace/tmpbuild/weed_phase1}
AGENT=${AGENT:-/tmp/csd-agent-sim}
WORK=${WORK:-/home/tan/new_workspace/tmpbuild/multinode}
CSV=${CSV:-$(cd "$(dirname "$0")/.." && pwd)/results/m3_multinode.csv}
IMAGES=${1:-2048}
K=${2:-5}
REPS=${REPS:-3}

MASTER_PORT=19500
FILER_PORT=19600
VOLUME_BASE=19700
AGENT_BASE=19800
VOLUME_COUNT=3

PIDS=()
VOLUME_PIDS=()
cleanup() {
    for pid in "${PIDS[@]:-}"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null
    done
    wait 2>/dev/null
}
trap cleanup EXIT

log() { printf '[multinode] %s\n' "$*" >&2; }

wait_http() { # url timeout_s
    local url="$1" deadline=$((SECONDS + $2))
    while [ $SECONDS -lt $deadline ]; do
        curl -s -o /dev/null "$url" && return 0
        sleep 0.3
    done
    return 1
}

rm -rf "$WORK"
mkdir -p "$WORK"/{master,filer,agents}
mkdir -p "$(dirname "$CSV")"

log "starting master"
"$WEED" master -mdir="$WORK/master" -port=$MASTER_PORT -ip=127.0.0.1 \
    -defaultReplication=001 -volumeSizeLimitMB=256 > "$WORK/master.log" 2>&1 &
PIDS+=($!)
wait_http "http://127.0.0.1:${MASTER_PORT}/dir/status" 20 || { log "master failed"; exit 1; }

for i in $(seq 0 $((VOLUME_COUNT - 1))); do
    volume_port=$((VOLUME_BASE + i))
    agent_port=$((AGENT_BASE + i))
    mkdir -p "$WORK/vol$i"
    log "starting agent $i on $agent_port"
    "$AGENT" -listen="127.0.0.1:${agent_port}" -mode=direct > "$WORK/agents/agent$i.log" 2>&1 &
    PIDS+=($!)
    log "starting volume $i on $volume_port"
    "$WEED" volume -dir="$WORK/vol$i" -port=$volume_port -mserver="127.0.0.1:${MASTER_PORT}" \
        -ip=127.0.0.1 -max=4 -volume.compute.enabled=true \
        -volume.compute.csd.enabled=true \
        -volume.compute.csd.endpoint="http://127.0.0.1:${agent_port}" \
        > "$WORK/vol$i.log" 2>&1 &
    PIDS+=($!)
    VOLUME_PIDS+=($!)
done
for i in $(seq 0 $((VOLUME_COUNT - 1))); do
    wait_http "http://127.0.0.1:$((VOLUME_BASE + i))/status" 20 || { log "volume $i failed"; exit 1; }
done

log "starting filer"
"$WEED" filer -master="127.0.0.1:${MASTER_PORT}" -port=$FILER_PORT -ip=127.0.0.1 \
    -defaultStoreDir="$WORK/filer" > "$WORK/filer.log" 2>&1 &
PIDS+=($!)
wait_http "http://127.0.0.1:${FILER_PORT}/" 25 || { log "filer failed"; exit 1; }

log "running compute workload (${IMAGES} images, top-${K}, ${REPS} reps) against this cluster"
# Drive the experiment directly against the cluster started above: the runner
# script would start (and tear down) a cluster of its own.
python3 "$(dirname "$0")/image_search_experiment.py" \
    --filer="http://127.0.0.1:${FILER_PORT}" --work="$WORK/imgsearch" \
    --images="$IMAGES" --k="$K" --reps="$REPS" --csv="$CSV" \
    2>&1 | grep -E "^\[sum\]|^\[ref\]|^\[up\]|^\[base\]" || true

local_volume_id() { # index -> volume id (from the .dat file name)
    local dir="$WORK/vol$1"
    local file
    file=$(find "$dir" -maxdepth 1 -name '*.dat' | head -1)
    [ -z "$file" ] && return 1
    basename "$file" .dat | awk -F_ '{print $NF}'
}

echo
echo "== retained data versions per volume server =="
for i in $(seq 0 $((VOLUME_COUNT - 1))); do
    volume_port=$((VOLUME_BASE + i))
    volume_id=$(local_volume_id "$i") || continue
    body=$(curl -s "http://127.0.0.1:${volume_port}/compute/versions?volume=${volume_id}")
    echo "volume-server:${volume_port} volume:${volume_id} ${body}"
done

echo
echo "== files on disk =="
find "$WORK" -name '*.dat*' | sort | sed "s|$WORK/||"

echo
echo "== forcing reclamation with a 1s retention on each volume server =="
for i in $(seq 0 $((VOLUME_COUNT - 1))); do
    volume_port=$((VOLUME_BASE + i))
    volume_id=$(local_volume_id "$i") || continue
    curl -s -X POST "http://127.0.0.1:${volume_port}/compute/versions?volume=${volume_id}&retention=1s"
    echo
done

echo
echo "== files on disk after reclamation =="
find "$WORK" -name '*.dat*' | sort | sed "s|$WORK/||"

if [ "${RUN_MATRIX:-0}" = "1" ]; then
    echo
    log "running the M4 fault-injection matrix"
    python3 "$(dirname "$0")/consistency_matrix.py" \
        --filer="http://127.0.0.1:${FILER_PORT}" \
        --agent="http://127.0.0.1:${AGENT_BASE}" \
        --work="$WORK/matrix" --images="${MATRIX_IMAGES:-1024}" --k="$K" \
        --csv="$(dirname "$CSV")/m4_matrix.csv" \
        --volume-pids="$(IFS=,; echo "${VOLUME_PIDS[*]}")" || true
fi

log "done; logs under $WORK"
