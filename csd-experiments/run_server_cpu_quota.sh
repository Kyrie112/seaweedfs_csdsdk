#!/usr/bin/env bash
# Server CPU-quota experiment: current server-side offload under CPU quota + concurrency
# Restarts .10 volume under a systemd CPUQuota scope, then measures ?compute=sum latency
# for N concurrent requests.
# Usage: ./run_server_cpu_quota.sh all <csv> | one <pct> <N> [reps]
set -u
export DISPLAY=:0 SSH_ASKPASS=/home/tan/new_workspace/.tools/askpass.sh SSH_ASKPASS_REQUIRE=force
SERVER=192.168.0.10
FILER=192.168.0.9
URL="http://192.168.0.9:8888/dataset/big_numbers.txt?compute=sum"
EXPECT_SUM=179986461084
WORK=/home/tan/new_workspace/work
SSH="ssh -o ConnectTimeout=8 -o StrictHostKeyChecking=no"
VOLUME_CMD="/home/dess/seaweed/weed volume -ip=192.168.0.10 -port=8080 -dir=/home/dess/seaweed/data -mserver=192.168.0.9:9333 -volume.compute.enabled=true -volume.compute.dir=/home/dess/compute_program -volume.compute.timeout=300s -volume.compute.maxOutputMB=16"

now() { date +%s.%N; }
math() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a-b}'; }

wait_vol_registered() {
    for i in $(seq 1 40); do
        curl -s -m 3 "http://192.168.0.9:9333/vol/status" 2>/dev/null | grep -q "192.168.0.10:8080" && return 0
        sleep 1
    done
    return 1
}

set_quota() { # pct
    local pct="$1"
    # stop previous quota unit and any volume process (no plain volume-cmd text in this ssh)
    $SSH dess@$SERVER "echo 'B511@TanServer' | sudo -S -p '' systemctl stop weedquota 2>/dev/null; echo 'B511@TanServer' | sudo -S -p '' systemctl reset-failed weedquota 2>/dev/null; pkill -f '[w]eed volume' 2>/dev/null; sleep 3; for i in \$(seq 1 30); do ss -ltn 2>/dev/null | grep -qE ':(8080|18080)' || break; sleep 1; done; echo stopped_ok" < /dev/null 2>/dev/null
    # start volume under CPUQuota (separate ssh; this string contains the plain volume cmd)
    $SSH dess@$SERVER "echo 'B511@TanServer' | sudo -S -p '' systemd-run --unit=weedquota -p CPUQuota=${pct}% --uid=dess --gid=dess -- $VOLUME_CMD >/dev/null 2>&1; echo quota_${pct}_launched" < /dev/null 2>/dev/null
    wait_vol_registered || { echo "ERR: volume not registered after quota change"; return 1; }
}

restore_volume() {
    $SSH dess@$SERVER "echo 'B511@TanServer' | sudo -S -p '' systemctl stop weedquota 2>/dev/null; echo 'B511@TanServer' | sudo -S -p '' systemctl reset-failed weedquota 2>/dev/null; pkill -f '[w]eed volume' 2>/dev/null; sleep 3" < /dev/null 2>/dev/null
    $SSH -f dess@$SERVER "cd /home/dess/seaweed && nohup setsid ./weed volume -ip=192.168.0.10 -port=8080 -dir=/home/dess/seaweed/data -mserver=192.168.0.9:9333 -volume.compute.enabled=true -volume.compute.dir=/home/dess/compute_program -volume.compute.timeout=300s -volume.compute.maxOutputMB=16 > logs/volume.log 2>&1 < /dev/null &" 2>/dev/null
    wait_vol_registered
}

concurrency_run() { # pct N rep
    local pct="$1" N="$2" rep="$3" t0 t1 wall ok=0 okc=0
    rm -f "$WORK"/csum_*.txt "$WORK"/ccode_*.txt
    t0=$(now)
    seq 1 "$N" | xargs -P "$N" -I{} bash -c "curl -s -m 600 -o '$WORK/csum_{}.txt' -w '%{http_code}' '$URL' > '$WORK/ccode_{}.txt'" 2>/dev/null
    t1=$(now)
    wall=$(math "$t1" "$t0")
    for i in $(seq 1 "$N"); do
        if [ "$(cat "$WORK/csum_$i.txt" 2>/dev/null)" = "$EXPECT_SUM" ] && [ "$(cat "$WORK/ccode_$i.txt" 2>/dev/null)" = "200" ]; then
            okc=$((okc+1))
        fi
    done
    [ "$okc" = "$N" ] && ok=1
    printf "%s,%s,%s,%.4f,%d,%d\n" "$pct" "$N" "$rep" "$wall" "$ok" "$okc"
}

if [ "${1:-}" = "one" ]; then
    pct="$2"; N="$3"; reps="${4:-3}"
    echo "pct,N,rep,wall_s,ok,ok_count"
    set_quota "$pct" || exit 1
    for rep in $(seq 1 "$reps"); do concurrency_run "$pct" "$N" "$rep"; done
    exit 0
fi

if [ "${1:-}" = "all" ]; then
    CSV="${2:-$WORK/results_server_cpu_quota.csv}"
    : > "$CSV"
    echo "pct,N,rep,wall_s,ok,ok_count" >> "$CSV"
    for pct in 100 200 3200; do
        set_quota "$pct" || { echo "quota $pct failed" >> "$CSV"; continue; }
        for N in 1 2 4; do
            for rep in $(seq 1 3); do
                concurrency_run "$pct" "$N" "$rep" >> "$CSV"
            done
            echo "done: quota=$pct N=$N at $(date +%T)" >> "$WORK/progress_quota.log"
        done
    done
    restore_volume
    echo "ALL DONE at $(date +%T)" >> "$WORK/progress_quota.log"
    exit 0
fi

echo "Usage: $0 all <csv> | one <pct> <N> [reps]"
exit 1
