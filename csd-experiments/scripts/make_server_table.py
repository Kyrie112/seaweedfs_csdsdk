import csv, statistics
rows = list(csv.DictReader(open("/home/tan/new_workspace/work/results_server_table.csv")))
def cell(row, col):
    t = [float(r["offload_s"]) for r in rows if r["row"]==row and r["col"]==col]
    ok = all(r["ok"]=="1" for r in rows if r["row"]==row and r["col"]==col)
    m = statistics.mean(t) if t else 0
    return m, ok, t
cols = ["unlim","50M","20M","10M"]
rows_l = ["unlim","2c","1c","1c_burn"]
label = {"unlim":"无限制","2c":"2 Core","1c":"1 Core","1c_burn":"1 Core+争抢"}
print("| Server CPU\\带宽 | 无限制 | 50Mbps | 20Mbps | 10Mbps |")
print("|---|---|---|---|---|")
for row in rows_l:
    cells = []
    for col in cols:
        m, ok, t = cell(row, col)
        if ok:
            cells.append(f"{m:.1f}s")
        else:
            cells.append(f"失败({min(t):.0f}~{max(t):.0f}s)")
    print(f"| {label[row]} | " + " | ".join(cells) + " |")
