#!/usr/bin/env bash
# SeaweedFS CSD experiments - SERVER-side resource limits
# Limits: Server(.10 volume) CPU via taskset + moderate burners; Server(.9 filer) upload BW via -downloadMaxMBps
# Modes: host (client download+sum), offload (?compute=sum on server)
# cpu: none|2c|1c ; bw: unlim|6M|3M|1M (MB/s)
#   1c_burn = volume pinned to core 0 + 16 busy burners on cores 0-15 (moderate contention)
set -u
export DISPLAY=:0 SSH_ASKPASS=/home/tan/new_workspace/.tools/askpass.sh SSH_ASKPASS_REQUIRE=force
URL="http://192.168.0.9:8888/dataset/big_numbers.txt"
COMPUTE_URL="$URL?compute=sum"
DL_FILE="/home/tan/new_workspace/work/dl.bin"
EXPECT_SUM=179986461084
SERVER=192.168.0.10
FILER_NODE=192.168.0.9
SSH="ssh -o ConnectTimeout=8 -o StrictHostKeyChecking=no"
NREP="${3:-3}"

now() { date +%s.%N; }
math() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a-b}'; }
add() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a+b}'; }

bw_flag() { case "$1" in unlim) echo 0;; 6M) echo 6;; 3M) echo 3;; 1M) echo 1;; *) echo 0;; esac; }

VOL_PID=""
get_vol_pid() {
    VOL_PID=$($SSH dess@$SERVER 'pgrep -f "[w]eed volume" | head -1' < /dev/null 2>/dev/null | tr -d '\r\n ')
}


set_server_cpu() { # none|2c|1c|1c_burn
    local cpu="$1"
    [ -z "$VOL_PID" ] && get_vol_pid
    case "$cpu" in
        none)    $SSH dess@$SERVER "taskset -pc 0-31 $VOL_PID >/dev/null 2>&1" < /dev/null 2>/dev/null;;
        2c)      $SSH dess@$SERVER "taskset -pc 0-1 $VOL_PID >/dev/null 2>&1" < /dev/null 2>/dev/null;;
        1c)      $SSH dess@$SERVER "taskset -pc 0 $VOL_PID >/dev/null 2>&1" < /dev/null 2>/dev/null;;
        
    esac
}

set_filer_bw() { # MB/s
    local mbs="$1"
    $SSH dess@$FILER_NODE "pkill -f '[w]eed filer' 2>/dev/null; for i in \$(seq 1 25); do ss -ltn 2>/dev/null | grep -q ':8888' || break; sleep 1; done" < /dev/null 2>/dev/null
    sleep 1
    $SSH -f dess@$FILER_NODE "cd /home/dess/seaweed && nohup setsid ./weed filer -ip=192.168.0.9 -port=8888 -master=192.168.0.9:9333 -defaultStoreDir=/home/dess/seaweed/filer -maxMB=1024 -downloadMaxMBps=$mbs > logs/filer.log 2>&1 < /dev/null &" 2>/dev/null
    for i in $(seq 1 20); do
        curl -s -m 2 -o /dev/null "http://192.168.0.9:8888/" && break
        sleep 1
    done
}

measure_host() { # cpu bw rep
    local cpu="$1" bw="$2" rep="$3" t0 t1 t_dl t_cpu t_total out ok
    t0=$(now); curl -s -o "$DL_FILE" "$URL"; t1=$(now)
    t_dl=$(math "$t1" "$t0")
    t0=$(now); out=$(awk '{s+=$1} END{print s}' "$DL_FILE"); t1=$(now)
    t_cpu=$(math "$t1" "$t0"); t_total=$(add "$t_dl" "$t_cpu")
    if [ "$out" = "$EXPECT_SUM" ]; then ok=1; else ok=0; fi
    printf "host,%s,%s,%s,%.4f,%.4f,%.4f,,%s,%d\n" "$cpu" "$bw" "$rep" "$t_dl" "$t_cpu" "$t_total" "$out" "$ok"
}

measure_offload() { # cpu bw rep
    local cpu="$1" bw="$2" rep="$3" t0 t1 out ok
    t0=$(now); out=$(curl -s -m 300 "$COMPUTE_URL"); t1=$(now)
    if [ "$out" = "$EXPECT_SUM" ]; then ok=1; else ok=0; fi
    printf "offload,%s,%s,%s,,,,%.4f,%s,%d\n" "$cpu" "$bw" "$rep" "$(math "$t1" "$t0")" "$out" "$ok"
}

if [ "${1:-}" = "one" ]; then
    mode="$2"; cpu="$3"; bw="$4"; reps="${5:-$NREP}"
    echo "mode,cpu,bw,rep,t_dl,t_cpu,t_total,t_off,sum,ok"
    get_vol_pid
    set_server_cpu "$cpu"
    set_filer_bw "$(bw_flag "$bw")"
    for rep in $(seq 1 "$reps"); do
        if [ "$mode" = "host" ]; then measure_host "$cpu" "$bw" "$rep"; else measure_offload "$cpu" "$bw" "$rep"; fi
    done
    set_server_cpu none
    set_filer_bw 0
    exit 0
fi

if [ "${1:-}" = "all" ]; then
    CSV="${2:-/home/tan/new_workspace/work/results_server.csv}"
    : > "$CSV"
    echo "mode,cpu,bw,rep,t_dl,t_cpu,t_total,t_off,sum,ok" >> "$CSV"
    get_vol_pid
    for bw in unlim 6M 3M 1M; do
        set_filer_bw "$(bw_flag "$bw")"
        for cpu in none 2c 1c; do
            set_server_cpu "$cpu"
            for rep in $(seq 1 "$NREP"); do
                measure_host "$cpu" "$bw" "$rep" >> "$CSV"
                measure_offload "$cpu" "$bw" "$rep" >> "$CSV"
            done
            echo "done: bw=$bw cpu=$cpu at $(date +%T)" >> /home/tan/new_workspace/work/progress_server.log
        done
    done
    set_server_cpu none
    set_filer_bw 0
    echo "ALL DONE at $(date +%T)" >> /home/tan/new_workspace/work/progress_server.log
    exit 0
fi

echo "Usage: $0 all <csv> [NREP] | one <mode> <cpu> <bw> [NREP]"
exit 1
