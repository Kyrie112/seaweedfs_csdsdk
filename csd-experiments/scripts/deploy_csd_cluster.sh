#!/usr/bin/env bash
# Deploy the CSD-enabled SeaweedFS cluster on remote hosts (designed for the
# tan09 / tan10 / tan11 machines used by this project).
#
# This script was written but could NOT be executed from the development
# container: tan09/tan10/tan11 do not resolve there and the container has no
# route to the 192.168.0.x network. Run it from a host that can reach them.
#
# Topology:
#   <host1>: master + filer
#   <host1..3>: volume server, each paired with a local CSD agent
#
# Usage:
#   HOSTS="tan09 tan10 tan11" ./deploy_csd_cluster.sh start|stop|status
set -u

HOSTS=${HOSTS:-"tan09 tan10 tan11"}
REMOTE_DIR=${REMOTE_DIR:-/home/tan/csd-cluster}
WEED_BIN=${WEED_BIN:-/home/tan/new_workspace/tmpbuild/weed_phase1}
AGENT_BIN=${AGENT_BIN:-/tmp/csd-agent-sim}
MASTER_PORT=9333
FILER_PORT=8888
VOLUME_PORT=8080
AGENT_PORT=18090
ACTION=${1:-status}

read -r -a HOST_ARRAY <<< "$HOSTS"
MASTER_HOST=${HOST_ARRAY[0]}

log() { printf '[deploy] %s\n' "$*" >&2; }

case "$ACTION" in
start)
    log "copying binaries to ${#HOST_ARRAY[@]} host(s)"
    for host in "${HOST_ARRAY[@]}"; do
        ssh "$host" "mkdir -p $REMOTE_DIR/{data,master,filer,log}"
        scp -q "$WEED_BIN" "$host:$REMOTE_DIR/weed"
        scp -q "$AGENT_BIN" "$host:$REMOTE_DIR/csd-agent-sim"
    done

    log "starting master + filer on $MASTER_HOST"
    ssh "$MASTER_HOST" "cd $REMOTE_DIR && nohup ./weed master -mdir=$REMOTE_DIR/master \
        -port=$MASTER_PORT -ip=$MASTER_HOST -defaultReplication=001 \
        > log/master.log 2>&1 &"
    ssh "$MASTER_HOST" "cd $REMOTE_DIR && nohup ./weed filer -master=$MASTER_HOST:$MASTER_PORT \
        -port=$FILER_PORT -ip=$MASTER_HOST -defaultStoreDir=$REMOTE_DIR/filer \
        > log/filer.log 2>&1 &"

    log "starting CSD agent + volume server on every host"
    for host in "${HOST_ARRAY[@]}"; do
        ssh "$host" "cd $REMOTE_DIR && chmod +x weed csd-agent-sim && \
            nohup ./csd-agent-sim -listen=0.0.0.0:$AGENT_PORT -mode=direct \
            > log/agent.log 2>&1 &"
        ssh "$host" "cd $REMOTE_DIR && nohup ./weed volume -dir=$REMOTE_DIR/data \
            -port=$VOLUME_PORT -mserver=$MASTER_HOST:$MASTER_PORT -ip=$host -max=8 \
            -volume.compute.enabled=true \
            -volume.compute.csd.enabled=true \
            -volume.compute.csd.endpoint=http://127.0.0.1:$AGENT_PORT \
            > log/volume.log 2>&1 &"
    done
    log "started; check with: HOSTS=\"$HOSTS\" $0 status"
    ;;
stop)
    for host in "${HOST_ARRAY[@]}"; do
        ssh "$host" "pkill -f 'csd-agent-sim' ; pkill -f './weed volume'" || true
    done
    ssh "$MASTER_HOST" "pkill -f './weed filer' ; pkill -f './weed master'" || true
    log "stopped"
    ;;
status)
    curl -s "http://$MASTER_HOST:$MASTER_PORT/dir/status" >/dev/null && log "master OK" || log "master unreachable"
    curl -s "http://$MASTER_HOST:$FILER_PORT/" >/dev/null && log "filer OK" || log "filer unreachable"
    for host in "${HOST_ARRAY[@]}"; do
        curl -s "http://$host:$VOLUME_PORT/status" >/dev/null && log "volume@$host OK" || log "volume@$host unreachable"
        curl -s "http://$host:$AGENT_PORT/v1/stats" | head -c 160; echo
    done
    ;;
verify)
    # Consistency + version-retention checks against the deployed cluster.
    log "running the consistency experiment against http://$MASTER_HOST:$FILER_PORT"
    SCRIPT=consistency_experiment.py \
        WEED="$WEED_BIN" AGENT="$AGENT_BIN" \
        ./run_image_search_experiment.sh 512 5
    for host in "${HOST_ARRAY[@]}"; do
        log "retained versions on $host"
        curl -s "http://$host:$VOLUME_PORT/compute/versions?volume=1"; echo
    done
    ;;
*)
    echo "usage: HOSTS=\"$HOSTS\" $0 start|stop|status|verify" >&2
    exit 2
    ;;
esac
