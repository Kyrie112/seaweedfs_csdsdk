import csv, statistics
def load(p):
    return list(csv.DictReader(open(p)))
grid = load("/home/tan/new_workspace/work/results_server.csv")
ch = load("/home/tan/new_workspace/work/confirm_server_host_2c_1M.csv")
co = load("/home/tan/new_workspace/work/confirm_server_offload_2c_1M.csv")
h = [float(r["t_total"]) for r in grid if r["mode"]=="host" and r["cpu"]=="2c" and r["bw"]=="1M"] + [float(r["t_total"]) for r in ch]
o = [float(r["t_off"]) for r in grid if r["mode"]=="offload" and r["cpu"]=="2c" and r["bw"]=="1M"] + [float(r["t_off"]) for r in co]
for name, vals in (("host", h), ("offload", o)):
    m = statistics.mean(vals); s = statistics.stdev(vals); print(f"{name}: n={len(vals)} mean={m:.3f}s std={s:.3f} cv={s/m*100:.2f}%")
print(f"speedup = {statistics.mean(h)/statistics.mean(o):.3f}x")
