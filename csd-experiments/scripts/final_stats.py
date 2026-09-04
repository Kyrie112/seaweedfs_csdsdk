import csv, statistics

def load(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            rows.append(r)
    return rows

grid = load("/home/tan/new_workspace/work/results.csv")
conf = {c: load(f"/home/tan/new_workspace/work/confirm_{c}.csv") for c in
        ["host_1c_burn_10M", "offload_1c_burn_10M", "host_1c_10M", "offload_1c_10M"]}

def stat(name):
    rs = [r for r in grid if r["mode"]+","+r["cpu"]+","+r["bw"] == name]
    rs += conf[name.replace(",", "_")]
    t = [float(r["t_total"]) if r["mode"] == "host" else float(r["t_off"]) for r in rs]
    mean = statistics.mean(t); std = statistics.stdev(t); cv = std/mean*100
    ok = all(r["ok"] == "1" for r in rs)
    print(f"{name:24s} n={len(rs):2d} mean={mean:8.3f}s std={std:6.3f} cv={cv:5.2f}% ok={ok}")
    return mean, cv

h1 = stat("host,1c_burn,10M")
o1 = stat("offload,1c_burn,10M")
h2 = stat("host,1c,10M")
o2 = stat("offload,1c,10M")
print(f"\nspeedup 1c_burn/10M = {h1[0]/o1[0]:.3f}x  (host_cv={h1[1]:.2f}%, off_cv={o1[1]:.2f}%)")
print(f"speedup 1c/10M      = {h2[0]/o2[0]:.3f}x  (host_cv={h2[1]:.2f}%, off_cv={o2[1]:.2f}%)")

# full table for report
print("\n=== FULL GRID (mean seconds) ===")
for cpu in ["none","2c","1c","1c_burn"]:
    line = []
    for bw in ["unlim","50M","20M","10M"]:
        h = [r for r in grid if r["mode"]=="host" and r["cpu"]==cpu and r["bw"]==bw]
        o = [r for r in grid if r["mode"]=="offload" and r["cpu"]==cpu and r["bw"]==bw]
        hm = statistics.mean(float(r["t_total"]) for r in h)
        om = statistics.mean(float(r["t_off"]) for r in o)
        line.append(f"{hm:.1f}/{om:.1f}({hm/om:.2f}x)")
    print(f"{cpu:8s} " + "  ".join(f"{x:>16s}" for x in line))
