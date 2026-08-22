#!/usr/bin/env bash
# SeaweedFS CSD experiments: host-side compute vs storage offload
# Usage:
#   ./run_experiments.sh all <csv> [NREP]
#   ./run_experiments.sh one <mode> <cpu> <bw> [NREP]   # mode=host|offload
# cpu: none|2c|1c|1c_burn ; bw: unlim|50M|20M|10M
set -u
URL="http://192.168.0.9:8888/dataset/big_numbers.txt"
COMPUTE_URL="$URL?compute=sum"
DL_FILE="/home/tan/new_workspace/work/dl.bin"
EXPECT_SUM=179986461084
WORK=/home/tan/new_workspace/work
BURNER="$WORK/burner.sh"
NREP="${3:-3}"

now() { date +%s.%N; }
math() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a-b}'; }
add() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a+b}'; }

bw_rate() {
    case "$1" in
        unlim) echo 0;;
        50M)   echo 6250000;;
        20M)   echo 2500000;;
        10M)   echo 1250000;;
        *)     echo 0;;
    esac
}

start_burn() {
    for i in $(seq 0 119); do taskset -c "$i" "$BURNER" & done
    sleep 1
}
stop_burn() {
    pkill -f "burner[.]sh" 2>/dev/null
    sleep 1
}

run_awk() { # cpu_level file
    case "$1" in
        none)    awk '{s+=$1} END{print s}' "$2";;
        2c)      taskset -c 0-1 awk '{s+=$1} END{print s}' "$2";;
        1c|1c_burn) taskset -c 0 awk '{s+=$1} END{print s}' "$2";;
    esac
}

measure_host() { # cpu bw rep
    local cpu="$1" bw="$2" rep="$3" rate t0 t1 t_dl t_cpu t_total out ok
    rate=$(bw_rate "$bw")
    if [ "$cpu" = "1c_burn" ]; then start_burn; fi
    t0=$(now)
    if [ "$rate" != "0" ]; then
        curl --limit-rate "$rate" -s -o "$DL_FILE" "$URL"
    else
        curl -s -o "$DL_FILE" "$URL"
    fi
    t1=$(now)
    t_dl=$(math "$t1" "$t0")
    t0=$(now)
    out=$(run_awk "$cpu" "$DL_FILE")
    t1=$(now)
    t_cpu=$(math "$t1" "$t0")
    t_total=$(add "$t_dl" "$t_cpu")
    if [ "$cpu" = "1c_burn" ]; then stop_burn; fi
    if [ "$out" = "$EXPECT_SUM" ]; then ok=1; else ok=0; fi
    printf "host,%s,%s,%s,%.4f,%.4f,%.4f,,%s,%d\n" "$cpu" "$bw" "$rep" "$t_dl" "$t_cpu" "$t_total" "$out" "$ok"
}

measure_offload() { # cpu bw rep
    local cpu="$1" bw="$2" rep="$3" rate t0 t1 out ok
    rate=$(bw_rate "$bw")
    if [ "$cpu" = "1c_burn" ]; then start_burn; fi
    t0=$(now)
    if [ "$rate" != "0" ]; then
        out=$(curl --limit-rate "$rate" -s -m 300 "$COMPUTE_URL")
    else
        out=$(curl -s -m 300 "$COMPUTE_URL")
    fi
    t1=$(now)
    if [ "$cpu" = "1c_burn" ]; then stop_burn; fi
    if [ "$out" = "$EXPECT_SUM" ]; then ok=1; else ok=0; fi
    printf "offload,%s,%s,%s,,,,%.4f,%s,%d\n" "$cpu" "$bw" "$rep" "$(math "$t1" "$t0")" "$out" "$ok"
}

if [ "${1:-}" = "one" ]; then
    mode="$2"; cpu="$3"; bw="$4"; reps="${5:-$NREP}"
    echo "mode,cpu,bw,rep,t_dl,t_cpu,t_total,t_off,sum,ok"
    for rep in $(seq 1 "$reps"); do
        if [ "$mode" = "host" ]; then measure_host "$cpu" "$bw" "$rep"; else measure_offload "$cpu" "$bw" "$rep"; fi
    done
    exit 0
fi

if [ "${1:-}" = "all" ]; then
    CSV="${2:-/home/tan/new_workspace/work/results.csv}"
    : > "$CSV"
    echo "mode,cpu,bw,rep,t_dl,t_cpu,t_total,t_off,sum,ok" >> "$CSV"
    for bw in unlim 50M 20M 10M; do
        for cpu in none 2c 1c 1c_burn; do
            for rep in $(seq 1 "$NREP"); do
                measure_host "$cpu" "$bw" "$rep" >> "$CSV"
                measure_offload "$cpu" "$bw" "$rep" >> "$CSV"
            done
            echo "done: bw=$bw cpu=$cpu at $(date +%T)" >> /home/tan/new_workspace/work/progress.log
        done
    done
    echo "ALL DONE at $(date +%T)" >> /home/tan/new_workspace/work/progress.log
    exit 0
fi

echo "Usage: $0 all <csv> [NREP] | one <mode> <cpu> <bw> [NREP]"
exit 1
