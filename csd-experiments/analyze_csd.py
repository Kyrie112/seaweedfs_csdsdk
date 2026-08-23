import csv, statistics
rows = list(csv.DictReader(open("/home/tan/new_workspace/work/results_csd_contention.csv")))
def agg(mode, cont):
    t = [float(r["time_s"]) for r in rows if r["mode"]==mode and r["contention"]==cont]
    ok = all(r["ok"]=="1" for r in rows if r["mode"]==mode and r["contention"]==cont)
    return statistics.mean(t), statistics.stdev(t), ok
print("| 争抢(burner数) | CSD file_sum64 (FPGA) | Server 端下沉 sum.sh | 主机 awk |")
print("|---|---|---|---|")
for cont in ["0","8","16","32"]:
    cells = []
    for mode in ["csd","offload","awk"]:
        m, s, ok = agg(mode, cont)
        cells.append(f"{m:.1f}±{s:.1f}s{' ✓' if ok else ' ✗失败'}")
    print(f"| {cont} | " + " | ".join(cells) + " |")
