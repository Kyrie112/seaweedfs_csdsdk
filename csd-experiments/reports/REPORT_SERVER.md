# Server 端资源限制下的计算下沉对比实验(SeaweedFS CSD)

## 1. 目的

延续"资源受限 → 需要近存计算"的思路,本轮把资源限制从客户端移到 **Server 端(存储节点)**:
限制存储服务器的 **CPU(绑核)** 与 **上传带宽**,验证当服务器资源紧张时,把计算放到数据所在处
(近存计算,模拟 CSD)的必要性。**真实 FPGA/CSD 计算任务不做**,用 volume 端 `?compute=sum`
(sum.sh)作为近存计算的载体,并用"服务器无限制时的 offload 耗时"作为 CSD 设备侧计算基线
(设备专用引擎不占用服务器 CPU)。

## 2. 环境与限制手段

- 集群:master+filer 在 192.168.0.9,volume 在 .9/.10/.11;测试文件 big_numbers.txt(176MB,
  3600 万个整数)位于 .10 的 volume 3;计算下沉参数 `-volume.compute.*` 与上一轮相同。
- **Server CPU 限制**:对 .10 的 weed volume 进程执行 `taskset -pc`(dess 可操作自有进程,无需 sudo):
  `none`=0-31 核、`2c`=0-1 核、`1c`=核 0。
- **Server 上传带宽限制**:重启 .9 的 filer 并设置 `-downloadMaxMBps`(0/6/3/1 MB/s),filer
  按请求精确限速(实测 2MB/s 档下载速率 2.02MB/s)。这是"服务器上传给客户端"的限速点。
- 尝试过的其他手段与结论:
  - `tc tbf` 在 .10 eno1 出口限速:filer 代理通路导致只有约一半字节经过被整形网卡,速率不可控,放弃;
  - Server 端 CPU 争抢(burner + 绑核):Go 编写的 volume(GOMAXPROCS=32)被钉到 1 核再叠加
    busy-loop,Go 调度器严重抖动,单请求从 ~17s 恶化到数分钟且 sshd 都无法响应,不可行;
    因此 Server CPU 限制采用"绑核"这一可控方式,并如实报告其影响(见 4.2)。

## 3. 方法

- **host(主机端计算)**:客户端从 filer 下载 176MB(受 Server 上传带宽与 CPU 绑核影响),
  再本地 `awk` 求和(客户端 CPU 不限制)。总耗时 = 下载 + 本地计算。
- **offload(近存计算模拟)**:客户端 `curl "...?compute=sum"`,sum.sh 在 .10 的 volume 进程内执行,
  只回传约 10 字节结果。
- **CSD 基线**:offload 在 (none, unlim) 的耗时 ≈ 17.49s,代表设备侧专用计算引擎不受服务器 CPU 影响。
- 网格:3 CPU 档 × 4 带宽档 × 3 次重复 × 2 模式 = 72 次测量;最优配置追加 5 次重复(合计 8 次)。
- 指标:端到端耗时(mean/std/CV)与结果正确性。

## 4. 结果

### 4.1 全网格(3 次重复均值,秒;格式 host/offload(加速比))

| CPU\带宽 | unlim | 6MB/s | 3MB/s | 1MB/s |
|---|---|---|---|---|
| none | 11.17/17.49 (0.64x) | 38.76/17.43 (2.22x) | 66.37/17.25 (3.85x) | 178.39/17.64 (10.11x) |
| 2c | 10.95/17.39 (0.63x) | 38.50/17.51 (2.20x) | 66.54/17.43 (3.82x) | 178.29/17.54 (10.16x) |
| 1c | 10.96/17.92 (0.61x) | 38.49/17.35 (2.22x) | 66.47/17.26 (3.85x) | 178.30/17.66 (10.10x) |

全部 72 次测量结果均正确(总和=179986461084),CV 绝大多数 <1%。

### 4.2 Server CPU 绑核的影响(如实记录)

- offload 在 none/2c/1c 下:17.49 / 17.39 / 17.92s,相对 CSD 基线 17.49s 的退化 0.99–1.02x
  —— **单请求场景下 Server CPU 绑核几乎无影响**(数据服务与 awk 均为单线程/IO 为主)。
- host 下载在同带宽档下三种 CPU 绑核耗时相同(如 1MB/s:178.39/178.29/178.30s)——瓶颈是带宽。
- 说明:Server CPU 的真实压力(多租户争抢)在本环境无法安全模拟(见 2),但带宽实验已经足以证明:
  **带宽是 Server 侧最敏感的资源**,数据搬运是主机端计算的瓶颈。

### 4.3 最优配置确认(合计 8 次重复)

| 配置 | host (s) | offload (s) | 加速比 | host CV | offload CV |
|---|---|---|---|---|---|
| **Server 2c + 1MB/s 上传限速** | **178.333 ± 0.063** | **17.525 ± 0.106** | **10.176x** | 0.04% | 0.61% |

## 5. 结论

1. **Server 上传带宽受限时,主机端计算线性恶化**:176MB 在 1MB/s 下仅下载约 171s,总耗时 178.3s;
   近存计算只回传结果,始终 ~17.5s,最高加速比 **10.18x**。
2. **Server CPU 绑核在单请求场景影响有限**(如实报告);即便 CPU 不受限,带宽受限已足以说明:
   服务器资源(尤其带宽)紧张时,"把数据搬到主机再计算"是昂贵路径,近存计算/CSD 是必要方案。
3. **真实 CSD(FPGA 引擎)不占用服务器 CPU、只回传结果**,正是应对"Server 资源被限制"场景的
   架构选择;本实验用 sum.sh 模拟了近存计算,得到的收益上界(10x)可直接指导 CSD 部署预期。

## 6. 复现

```bash
../scripts/run_server_experiments.sh all ../results/results_server.csv 3   # 全网格
../scripts/run_server_experiments.sh one host 2c 1M 5           # 最优配置确认
python3 ../scripts/analyze_server.py                               # 汇总统计与加速比
```

- 脚本:`../scripts/run_server_experiments.sh`(Server 端实验驱动)
- 数据:`../results/results_server.csv`(72 行)、`../results/confirm_server_host_2c_1M.csv`、`../results/confirm_server_offload_2c_1M.csv`
- 分析:`../scripts/analyze_server.py`、`../scripts/final_server_stats.py`
