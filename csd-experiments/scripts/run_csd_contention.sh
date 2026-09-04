#!/usr/bin/env bash
# CSD vs server-side offload vs host awk under Server CPU contention (.10)
# contention: 0|8|16|32 persistent burners (systemd services)
set -u
export DISPLAY=:0 SSH_ASKPASS=/home/tan/new_workspace/.tools/askpass.sh SSH_ASKPASS_REQUIRE=force
SERVER=192.168.0.10
FILER=192.168.0.9
SSH="ssh -o ConnectTimeout=8 -o StrictHostKeyChecking=no"
FILE=/home/dess/Dess/data/big_numbers.txt
XCLBIN=/home/dess/Dess/file_sum64/build/file_sum64.xclbin
HOST=/home/dess/Dess/file_sum64/build/host_file_sum64
COMPUTE_URL="http://192.168.0.9:8888/dataset/big_numbers.txt?compute=sum"
EXPECT_OFFLOAD=179986461084
WORK=/home/tan/new_workspace/work

now() { date +%s.%N; }
math() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a-b}'; }

set_burners() { # n
    local n="$1" i
    $SSH dess@$SERVER 'for i in $(seq 1 32); do echo "B511@TanServer" | sudo -S -p "" systemctl stop burner$i 2>/dev/null; done; pkill -f "burner[.]sh" 2>/dev/null; sleep 1' < /dev/null 2>/dev/null
    if [ "$n" -gt 0 ]; then
        $SSH dess@$SERVER "for i in \$(seq 1 $n); do echo 'B511@TanServer' | sudo -S -p '' systemd-run --unit=burner\$i --uid=dess --gid=dess -- /home/dess/burner.sh >/dev/null 2>&1; done; sleep 2" < /dev/null 2>/dev/null
    fi
    sleep 2
}

measure_csd() { # rep
    local rep="$1" t0 t1 out ok=0
    t0=$(now)
    out=$($SSH dess@$SERVER 'source /opt/xilinx/xrt/setup.sh >/dev/null 2>&1 && /home/dess/Dess/file_sum64/build/host_file_sum64 /home/dess/Dess/file_sum64/build/file_sum64.xclbin /home/dess/Dess/data/big_numbers.txt' < /dev/null 2>&1)
    t1=$(now)
    echo "$out" | grep -q "TEST PASSED" && ok=1
    printf "csd,%s,%s,%.4f,%d,%s\n" "$CONT" "$rep" "$(math "$t1" "$t0")" "$ok" "$(echo "$out" | grep 'device sum' | head -1)"
}

measure_offload() { # rep
    local rep="$1" t0 t1 out ok=0
    t0=$(now)
    out=$(curl -s -m 120 "$COMPUTE_URL")
    t1=$(now)
    [ "$out" = "$EXPECT_OFFLOAD" ] && ok=1
    printf "offload,%s,%s,%.4f,%d,%s\n" "$CONT" "$rep" "$(math "$t1" "$t0")" "$ok" "$(echo "$out" | head -c 40)"
}

measure_awk() { # rep
    local rep="$1" t0 t1 out ok=0
    t0=$(now)
    out=$($SSH dess@$SERVER "awk '{s+=\$1} END{print s}' $FILE" < /dev/null 2>&1)
    t1=$(now)
    [ "$(echo "$out" | tail -1)" = "$EXPECT_OFFLOAD" ] && ok=1
    printf "awk,%s,%s,%.4f,%d,%s\n" "$CONT" "$rep" "$(math "$t1" "$t0")" "$ok" "$(echo "$out" | tail -1)"
}

CSV="${1:-$WORK/results_csd_contention.csv}"
: > "$CSV"
echo "mode,contention,rep,time_s,ok,result" >> "$CSV"
for CONT in 0 8 16 32; do
    set_burners "$CONT"
    echo "burners=$CONT ready at $(date +%T)" >> "$WORK/progress_csd.log"
    for rep in 1 2 3; do
        measure_csd "$rep" >> "$CSV"
        measure_offload "$rep" >> "$CSV"
        measure_awk "$rep" >> "$CSV"
    done
    echo "done: contention=$CONT at $(date +%T)" >> "$WORK/progress_csd.log"
done
set_burners 0
echo "ALL DONE at $(date +%T)" >> "$WORK/progress_csd.log"
