#!/usr/bin/env bash
# Server-side table experiment v2 (corrected):
#   rows  = Server CPU: unlim(quota3200) | 2c(quota200) | 1c(quota100) | 1c_burn(quota100 + 32 persistent burners)
#   cols  = Server upload BW via tc on filer .9 eno1: unlim | 50M | 20M | 10M
#   cell  = offload latency (compute=sum)
# Note: taskset pinning is ineffective for the Go volume (threads/children don't
# inherit main-thread affinity), so CPU limits use cgroup CPUQuota via systemd-run.
set -u
export DISPLAY=:0 SSH_ASKPASS=/home/tan/new_workspace/.tools/askpass.sh SSH_ASKPASS_REQUIRE=force
URL="http://192.168.0.9:8888/dataset/big_numbers.txt?compute=sum"
EXPECT_SUM=179986461084
SERVER=192.168.0.10
FILER=192.168.0.9
WORK=/home/tan/new_workspace/work
SSH="ssh -o ConnectTimeout=8 -o StrictHostKeyChecking=no"
VOL_CMD="/home/dess/seaweed/weed volume -ip=192.168.0.10 -port=8080 -dir=/home/dess/seaweed/data -mserver=192.168.0.9:9333 -volume.compute.enabled=true -volume.compute.dir=/home/dess/compute_program -volume.compute.timeout=300s -volume.compute.maxOutputMB=16"
TOUT=120

now() { date +%s.%N; }
math() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a-b}'; }

stop_burners() {
    $SSH dess@$SERVER 'for i in $(seq 1 32); do echo "B511@TanServer" | sudo -S -p "" systemctl stop burner$i 2>/dev/null; done; pkill -f "burner[.]sh" 2>/dev/null; sleep 1' < /dev/null 2>/dev/null
}
start_burners() {
    $SSH dess@$SERVER 'for i in $(seq 1 32); do echo "B511@TanServer" | sudo -S -p "" systemd-run --unit=burner$i --uid=dess --gid=dess -- /home/dess/burner.sh >/dev/null 2>&1; done; sleep 2' < /dev/null 2>/dev/null
}
stop_volume() {
    $SSH dess@$SERVER "echo 'B511@TanServer' | sudo -S -p '' systemctl stop weedquota 2>/dev/null; echo 'B511@TanServer' | sudo -S -p '' systemctl reset-failed weedquota 2>/dev/null; pkill -f '[w]eed volume' 2>/dev/null; for i in \$(seq 1 30); do ss -ltn 2>/dev/null | grep -qE ':(8080|18080)' || break; sleep 1; done; echo ports_free" < /dev/null 2>/dev/null
}
start_volume_quota() { # pct
    local pct="$1"
    $SSH dess@$SERVER "echo 'B511@TanServer' | sudo -S -p '' systemd-run --unit=weedquota -p CPUQuota=${pct}% --uid=dess --gid=dess -- $VOL_CMD >/dev/null 2>&1; echo started_q${pct}" < /dev/null 2>/dev/null
}
wait_vol() {
    for i in $(seq 1 60); do
        curl -s -m 3 "http://192.168.0.9:9333/vol/status" 2>/dev/null | grep -q "192.168.0.10:8080" && return 0
        sleep 1
    done
    return 1
}
set_cpu() { # unlim|2c|1c|1c_burn
    local row="$1" pct=3200
    case "$row" in 2c) pct=200;; 1c|1c_burn) pct=100;; esac
    stop_burners
    stop_volume
    start_volume_quota "$pct"
    [ "$row" = "1c_burn" ] && start_burners
    wait_vol || echo "WARN: volume not registered for row=$row"
}
set_bw() { # unlim|50M|20M|10M
    local col="$1" mbit=0
    case "$col" in 50M) mbit=50;; 20M) mbit=20;; 10M) mbit=10;; esac
    if [ "$mbit" = "0" ]; then
        $SSH dess@$FILER "echo 'B511@TanServer' | sudo -S -p '' tc qdisc del dev eno1 root 2>/dev/null; echo ok" < /dev/null 2>/dev/null
    else
        $SSH dess@$FILER "echo 'B511@TanServer' | sudo -S -p '' tc qdisc del dev eno1 root 2>/dev/null; echo 'B511@TanServer' | sudo -S -p '' tc qdisc add dev eno1 root handle 1: tbf rate ${mbit}mbit burst 32kbit latency 400ms; echo ok" < /dev/null 2>/dev/null
    fi
    sleep 1
}
measure() { # row col rep
    local row="$1" col="$2" rep="$3" t0 t1 out ok=0
    t0=$(now); out=$(curl -s -m "$TOUT" "$URL"); t1=$(now)
    [ "$out" = "$EXPECT_SUM" ] && ok=1
    printf "%s,%s,%s,%.4f,%d\n" "$row" "$col" "$rep" "$(math "$t1" "$t0")" "$ok"
}

CSV="${1:-$WORK/results_server_table.csv}"
: > "$CSV"
echo "row,col,rep,offload_s,ok" >> "$CSV"
for col in unlim 50M 20M 10M; do
    set_bw "$col"
    for row in unlim 2c 1c 1c_burn; do
        set_cpu "$row"
        reps=3; [ "$row" = "1c_burn" ] && reps=2
        for rep in $(seq 1 "$reps"); do measure "$row" "$col" "$rep" >> "$CSV"; done
        echo "done: bw=$col row=$row at $(date +%T)" >> "$WORK/progress_table2.log"
    done
done
# restore: normal volume (no quota), no burners, unlim bw
stop_burners
stop_volume
$SSH -f dess@$SERVER "cd /home/dess/seaweed && nohup setsid ./weed volume -ip=192.168.0.10 -port=8080 -dir=/home/dess/seaweed/data -mserver=192.168.0.9:9333 -volume.compute.enabled=true -volume.compute.dir=/home/dess/compute_program -volume.compute.timeout=300s -volume.compute.maxOutputMB=16 > logs/volume.log 2>&1 < /dev/null &" 2>/dev/null
set_bw unlim
echo "ALL DONE at $(date +%T)" >> "$WORK/progress_table2.log"
