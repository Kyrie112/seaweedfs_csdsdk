#!/usr/bin/env python3
import csv, statistics, sys

path = sys.argv[1] if len(sys.argv) > 1 else "/home/tan/new_workspace/work/results.csv"
rows = []
with open(path) as f:
    for r in csv.DictReader(f):
        rows.append(r)

def val(r, k):
    return float(r[k]) if r.get(k) else 0.0

groups = {}
for r in rows:
    key = (r["mode"], r["cpu"], r["bw"])
    groups.setdefault(key, []).append(r)

print(f"{'mode':8s} {'cpu':8s} {'bw':6s} {'reps':>4s} {'mean_s':>9s} {'std_s':>8s} {'cv%':>6s} {'sum_ok':>6s}")
summary = []
for (mode, cpu, bw), rs in sorted(groups.items()):
    t = [val(r, "t_total") if mode == "host" else val(r, "t_off") for r in rs]
    mean = statistics.mean(t)
    std = statistics.stdev(t) if len(t) > 1 else 0.0
    cv = std / mean * 100 if mean else 0
    ok = all(r["ok"] == "1" for r in rs)
    print(f"{mode:8s} {cpu:8s} {bw:6s} {len(rs):>4d} {mean:9.3f} {std:8.3f} {cv:6.2f} {str(ok):>6s}")
    summary.append({"mode": mode, "cpu": cpu, "bw": bw, "mean": mean, "std": std, "cv": cv, "ok": ok})

# Build host vs offload comparison per (cpu, bw)
print("\n=== Host vs Offload (mean seconds) ===")
print(f"{'cpu':8s} {'bw':6s} {'host_s':>9s} {'off_s':>9s} {'speedup':>8s}")
comp = []
for cpu in ["none", "2c", "1c", "1c_burn"]:
    for bw in ["unlim", "50M", "20M", "10M"]:
        h = next((s for s in summary if s["mode"] == "host" and s["cpu"] == cpu and s["bw"] == bw), None)
        o = next((s for s in summary if s["mode"] == "offload" and s["cpu"] == cpu and s["bw"] == bw), None)
        if h and o:
            sp = h["mean"] / o["mean"] if o["mean"] else float("inf")
            print(f"{cpu:8s} {bw:6s} {h['mean']:9.3f} {o['mean']:9.3f} {sp:8.3f}")
            comp.append({"cpu": cpu, "bw": bw, "host": h["mean"], "off": o["mean"],
                         "speedup": sp, "host_cv": h["cv"], "off_cv": o["cv"],
                         "ok": h["ok"] and o["ok"]})

# Best: max speedup, both ok, both CV < 20%
cand = [c for c in comp if c["ok"] and c["host_cv"] < 20 and c["off_cv"] < 20]
cand.sort(key=lambda c: c["speedup"], reverse=True)
print("\n=== Best stable config (speedup, host_cv<20%, off_cv<20%) ===")
for c in cand[:5]:
    print(f"cpu={c['cpu']:8s} bw={c['bw']:6s} speedup={c['speedup']:.3f}x host={c['host']:.3f}s cv={c['host_cv']:.1f}% off={c['off']:.3f}s cv={c['off_cv']:.1f}%")
