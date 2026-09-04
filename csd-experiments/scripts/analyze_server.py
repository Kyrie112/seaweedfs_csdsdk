import csv, statistics

rows = list(csv.DictReader(open("/home/tan/new_workspace/work/results_server.csv")))
def val(r, k): return float(r[k]) if r.get(k) else 0.0

groups = {}
for r in rows:
    groups.setdefault((r["mode"], r["cpu"], r["bw"]), []).append(r)

print("=== Host vs Offload (mean seconds), server-side limits ===")
print(f"{'cpu':8s} {'bw':6s} {'host_s':>9s} {'off_s':>9s} {'speedup':>8s} {'host_cv%':>8s} {'off_cv%':>8s} {'ok':>4s}")
comp = []
for cpu in ["none", "2c", "1c"]:
    for bw in ["unlim", "6M", "3M", "1M"]:
        h = [val(r, "t_total") for r in groups.get(("host", cpu, bw), [])]
        o = [val(r, "t_off") for r in groups.get(("offload", cpu, bw), [])]
        if not h or not o: continue
        hm, om = statistics.mean(h), statistics.mean(o)
        hcv = statistics.stdev(h)/hm*100 if len(h) > 1 else 0
        ocv = statistics.stdev(o)/om*100 if len(o) > 1 else 0
        ok = all(r["ok"] == "1" for r in groups.get(("host", cpu, bw), []) + groups.get(("offload", cpu, bw), []))
        sp = hm/om
        print(f"{cpu:8s} {bw:6s} {hm:9.3f} {om:9.3f} {sp:8.3f} {hcv:8.2f} {ocv:8.2f} {str(ok):>4s}")
        comp.append(dict(cpu=cpu, bw=bw, h=hm, o=om, sp=sp, hcv=hcv, ocv=ocv, ok=ok))

cand = [c for c in comp if c["ok"] and c["hcv"] < 20 and c["ocv"] < 20]
cand.sort(key=lambda c: c["sp"], reverse=True)
print("\n=== Best stable config ===")
for c in cand[:3]:
    print(f"cpu={c['cpu']} bw={c['bw']} speedup={c['sp']:.3f}x host={c['h']:.3f}s cv={c['hcv']:.2f}% off={c['o']:.3f}s cv={c['ocv']:.2f}%")

# CSD baseline: offload at (none, unlim)
base = statistics.mean([val(r, "t_off") for r in groups.get(("offload", "none", "unlim"), [])])
print(f"\nCSD baseline (offload none/unlim) = {base:.3f}s")
for cpu in ["none", "2c", "1c"]:
    o = [val(r, "t_off") for r in groups.get(("offload", cpu, "unlim"), [])]
    print(f"server cpu {cpu:4s}: offload {statistics.mean(o):.3f}s vs CSD baseline {base:.3f}s (degradation {statistics.mean(o)/base:.2f}x)")
