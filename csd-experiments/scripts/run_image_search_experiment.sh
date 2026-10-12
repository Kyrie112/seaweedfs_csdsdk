#!/usr/bin/env bash
# Deploy a local SeaweedFS cluster + CSD agent and run the near-storage image
# similarity search experiment.
#
# Usage:
#   WEED=/path/to/weed AGENT=/path/to/csd-agent-sim ./run_image_search_experiment.sh [images] [k]
set -u

WEED=${WEED:-/home/tan/new_workspace/tmpbuild/weed_phase1}
AGENT=${AGENT:-/tmp/csd-agent-sim}
WORK=${WORK:-/tmp/csd-imgsearch}
CSV=${CSV:-$(cd "$(dirname "$0")/.." && pwd)/results/image_search.csv}
IMAGES=${1:-4096}
K=${2:-5}
SCRIPT=${SCRIPT:-image_search_experiment.py}
EXTRA=${EXTRA:-}
AGENT_ARGS=${AGENT_ARGS:-}
REPS=${REPS:-7}
READ_MODE=${READ_MODE:-direct}

MASTER_PORT=19334
VOLUME_PORT=18081
FILER_PORT=18889
AGENT_PORT=18091
AGENT_URL="http://127.0.0.1:${AGENT_PORT}"

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

log() { printf '[imgsearch] %s\n' "$*" >&2; }

wait_http() { # url timeout_s
    local url="$1" deadline=$((SECONDS + $2))
    while [ $SECONDS -lt $deadline ]; do
        if curl -s -o /dev/null "$url"; then return 0; fi
        sleep 0.3
    done
    return 1
}

if [ ! -x "$WEED" ]; then log "weed binary not found: $WEED"; exit 2; fi
if [ ! -x "$AGENT" ]; then log "agent binary not found: $AGENT"; exit 2; fi

rm -rf "$WORK"
mkdir -p "$WORK/master" "$WORK/data" "$WORK/filer" "$WORK/scripts"
mkdir -p "$(dirname "$CSV")"

log "starting master / volume / filer / agent"
"$WEED" master -mdir="$WORK/master" -port=$MASTER_PORT -ip=127.0.0.1 \
    -defaultReplication=000 -volumeSizeLimitMB=256 > "$WORK/master.log" 2>&1 &
MASTER_PID=$!
wait_http "http://127.0.0.1:${MASTER_PORT}/dir/status" 20 || { log "master failed"; cat "$WORK/master.log"; exit 1; }

"$WEED" volume -dir="$WORK/data" -port=$VOLUME_PORT -mserver="127.0.0.1:${MASTER_PORT}" \
    -ip=127.0.0.1 -max=4 -volume.compute.enabled=true \
    -volume.compute.dir="$WORK/scripts" \
    -volume.compute.csd.enabled=true \
    -volume.compute.csd.endpoint="$AGENT_URL" \
    > "$WORK/volume.log" 2>&1 &
VOLUME_PID=$!
wait_http "http://127.0.0.1:${VOLUME_PORT}/status" 20 || { log "volume failed"; cat "$WORK/volume.log"; exit 1; }

"$WEED" filer -master="127.0.0.1:${MASTER_PORT}" -port=$FILER_PORT -ip=127.0.0.1 \
    -defaultStoreDir="$WORK/filer" > "$WORK/filer.log" 2>&1 &
FILER_PID=$!
wait_http "http://127.0.0.1:${FILER_PORT}/" 25 || { log "filer failed"; cat "$WORK/filer.log"; exit 1; }

"$AGENT" -listen="127.0.0.1:${AGENT_PORT}" -mode="$READ_MODE" $AGENT_ARGS > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!
wait_http "$AGENT_URL/v1/stats" 15 || { log "agent failed"; cat "$WORK/agent.log"; exit 1; }

log "running experiment (images=$IMAGES k=$K read_mode=$READ_MODE)"
python3 "$(dirname "$0")/$SCRIPT" \
    --filer="http://127.0.0.1:${FILER_PORT}" \
    --work="$WORK" --images="$IMAGES" --k="$K" --reps="$REPS" --csv="$CSV" $EXTRA
status=$?
log "experiment finished with status $status"
exit $status
