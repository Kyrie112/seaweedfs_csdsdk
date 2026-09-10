# 阶段一:P2P 对齐通路与调度加固 —— 实施、部署与验证报告

本报告对应 [ROADMAP.md](../ROADMAP.md) 的阶段一工作:修复已定位的缺陷、
补齐分阶段耗时观测,并**在本地真实部署(master + volume + filer + CSD agent)
上做性能实验**。所有改动均已提交,实验可一键复现。

## 1. 结论摘要

| 工作项 | 状态 | 结论 |
| --- | --- | --- |
| P1-1 对齐读通路(含内核 v2) | 已实现并在真实布局上验证 | 真实 needle 请求从此真正走 O_DIRECT,结果与修复前逐字节一致 |
| P1-2 短读/部分读校验 | 已完成 | 短读不再被当作成功;只容忍窗口越过文件末尾的部分 |
| P1-3 调度器加固 | 已完成 | 探测不再持锁、失败副本被排除、负载按字节加权 |
| P1-4 分阶段耗时观测 | 已完成 | dispatch / 设备读 / 内核三段耗时可按 chunk 导出 |
| 本地部署 + 性能实验 | 已完成 | 64MB/16 chunk,O_DIRECT 全命中;冷缓存下时延 −27% |
| 自适应数据通路选择原型 | 已完成并量化 | 热场景 6.2× 加速,冷场景代价 ≈0;含「无法自举」的反面结论 |
| 真机 xclbin 重合成与端到端实测 | 待执行(需 .9 机器) | 命令与验收标准见第 8 节 |

两个核心结论:

1. **“P2P + O_DIRECT”在过去从未对真实业务请求生效**——判定条件对真实 needle
   布局几乎恒为假,修复后实测每个 chunk 都命中了 O_DIRECT。
2. **修复是对称的**:冷缓存下该路径显著更快(端到端中位时延 257ms → 188ms),
   但数据已驻留主机页缓存时反而更慢(32ms → 174ms)。这不是缺陷,而是
   O_DIRECT 的固有语义,直接给出了阶段二的“自适应路径选择”课题。

## 2. 背景:五个已定位的缺陷

前三个来自代码分析,后两个是**本次实际部署实验中暴露出来的**。

| # | 缺陷 | 影响 | 位置(修复前) |
| --- | --- | --- | --- |
| 1 | `O_DIRECT` 判定恒为假 | 近存计算“零拷贝”卖点未兑现 | `smartssd_compute_module/src/csd_agent.cpp:198` |
| 2 | 短读未校验 | 读到部分数据时静默返回错误结果 | `csd_agent.cpp:206` |
| 3 | 调度器锁粒度过大 / 重试无排除集 / 负载按请求计数 | 探测串行化;failover 可能重选失败副本 | `weed/server/filer_csd_scheduler.go` |
| 4 | 对齐窗口越过 `.dat` 文件末尾时被判为短读 | 卷文件中**最后一个 needle** 的请求会间歇性失败 | `aligned_range.h` / `csd_agent.cpp` |
| 5 | 在 O_DIRECT 描述符上做尾部缓冲读 | `EINVAL`,`read: invalid argument` | `aligned_range.h` / `csd_agent.cpp` |

### 2.1 对齐缺陷的根因

```text
DataOffset = actualOffset + NeedleHeaderSize + DataSizeSize = actualOffset + 20
```

needle 在 `.dat` 中无填充对齐,`actualOffset` 也不保证对齐。因此
`offset % 512 == 0 && size % 512 == 0` 对真实业务请求几乎恒为假。
此前 README 中 “512B O_DIRECT + P2P:期望 1760,返回 1760” 之所以成立,
是因为那是**人工构造**的对齐请求。

## 3. 修复设计

### 3.1 P1-1 对齐读通路

**为什么不能只在主机侧裁掉窗口头部的多余字节。** 内核把缓冲区按小端 `uint64`
打包求和,其值等于

```text
S = Σ_k b_k · 256^(k mod 8)      (k 为相对缓冲区起始的字节下标)
```

若缓冲区起点比 payload 提前 `δ = payload_shift` 字节,payload 第 `j` 字节落在窗口
下标 `δ + j` 处,直接求和得到 `Σ_j b_j · 256^((δ+j) mod 8)`——**只要 δ mod 8 ≠ 0,
每个 payload 字节的位权都变了**,不是减去前缀和能修正的。真实布局恰好如此
(`needleOffset = 0` 时 δ = 20,20 mod 8 = 4)。因此只有两条路:主机侧再搬一次
(等于放弃“无主机拷贝”),或把偏移交给设备。本工作选择后者。

**协议扩展**(旧 agent 忽略未知字段后仍按原语义工作):

```json
{
  "operation": "rawsum64",
  "data_file": "/var/lib/seaweedfs/2.dat",
  "offset": 255348, "size": 8388608,
  "read_offset": 254976, "read_size": 8389120, "payload_shift": 372
}
```

**内核 v2** 新增 `payload_shift` / `payload_size`,打包从 payload 首字节起算;
`payload_shift = 0, payload_size = byte_count` 时与 v1 逐位等价。
agent 启动时用 `CL_KERNEL_NUM_ARGS` 探测:3 参数按 v1、5 参数按 v2,
其它参数个数直接报错,避免“用错参数算出错误结果”。

### 3.2 P1-2 读取完整性

`read_fully_with` 循环读满,EOF 返回短计数;调用方比较
`got != available` 才报错,并计入 `short_reads`。

### 3.3 P1-3 调度器

| 问题 | 修复 |
| --- | --- |
| `csdSchedMu` 覆盖网络探测 | 探测移到锁外,临界区只做“排序内存快照 + 预留” |
| 重试可能选回失败副本 | 排除集保证每次重试命中未失败副本;全部排除则显式报错 |
| 负载按请求数计 | 主信号改为在途字节,请求数降为次级信号 |

排序目标:`CSD 能力(硬约束) → 在途字节 → 在途请求数 → 探测延迟 → host 字典序`。

### 3.4 P1-4 分阶段耗时

agent 返回 `read_ms` / `kernel_ms` / `direct` → volume 转
`X-CSD-Direct` / `X-CSD-Read-Ms` / `X-CSD-Kernel-Ms` → filer 汇总为
`X-Compute-Chunks` / `-Dispatch-Ms` / `-Device-Read-Ms` / `-Kernel-Ms` /
`-Direct-Hits` / `-Payload-Bytes`。本次实验的全部时延数据都取自这些字段。

## 4. 部署中发现并修复的两个缺陷

### 4.1 对齐窗口越过文件末尾

对齐窗口向上取整到 512 字节边界,而卷 `.dat` 文件恰好结束在最后一个 needle 的
最后一个字节上,因此**最后一个 needle 的窗口必然越过 EOF**。

冒烟测试立刻复现:期望 4608 字节、实际只有 4116 字节 → 被判为短读并失败。
修复:`available_in_window()` 按文件大小截断窗口,只要求
`payload_shift + payload_size ≤ available`,即**只有 payload 本身不完整才算错误**。

### 4.2 O_DIRECT 描述符上的缓冲尾读

窗口长度可能不是 512 的整数倍,尾部残块需要缓冲读;但在 O_DIRECT 描述符上
执行普通 `pread` 会返回 `EINVAL`(实测报错 `read: invalid argument`)。
修复:同时保留一个普通描述符,对齐部分走 O_DIRECT、尾部残块走缓冲读。

## 5. 本地部署与性能实验

由于本容器没有 XRT/FPGA,实验用**软件 CSD agent**
(`csd-experiments/tools/csd-agent-sim`)代替设备:它复用与真实 agent 完全相同的
`POST /v1/compute` 契约与求和语义(含 `payload_shift`),并支持三种读路径,
但用主机 CPU 计算,因此可以在一台普通机器上跑完整条链路。

### 5.1 实验方法

- 部署:`weed master` + `weed volume -volume.compute.csd.enabled=true` + `weed filer`,
  复制因子 000(单副本),数据 64MB → 被切成 **16 个 chunk(每块 4MB)**,
  因此每次请求都会真实走多 chunk 扇出与聚合;
- 三种读路径(每次切换 agent 模式并重启):
  - `legacy`:按 `[offset,size)` 原样缓冲读(v1 语义,读不到对齐窗口);
  - `aligned`:读对齐窗口 + 按 `payload_shift` 打包(缓冲读);
  - `direct`:读对齐窗口 + `O_DIRECT` + 按 `payload_shift` 打包(修复后的路径);
- 缓存条件:`warm`(页缓存已热)/ `cold`(每次请求前用
  `fadvise(DONTNEED)` 只驱逐该文件的页缓存,避免影响整机);
- 每个配置 1 次预热 + **9 次计时**,取中位数;
- 脚本:`csd-experiments/scripts/run_phase1_local.sh`,
  原始数据:`csd-experiments/results/phase1_local.csv`。

### 5.2 正确性(全部 PASS)

- 54 次请求(6 配置 × 9 次)全部 HTTP 200,返回**同一数值**
  `190022035469879755652`;
- **单 chunk 独立验证**:2MB 文件,SeaweedFS 结果 == 独立 Python 实现结果
  (`7256256511392428663`),且该请求 `direct_hits = 1`;
- **16 chunk 聚合独立验证**:按“每 chunk 求和后取模 2^64、聚合不再取模”的
  真实语义独立复算,结果完全一致(`190022035469879755652`);
- 三种读路径结果完全一致 ⇒ 对齐窗口 + `payload_shift` 的语义等价性在
  **真实 needle 布局**上得到端到端确认。

### 5.3 性能结果

时延为单次 compute 请求(64MB / 16 chunk)的中位数,单位 ms:

| 读路径 | warm(页缓存热) | cold(页缓存冷) |
| --- | --- | --- |
| legacy(修复前语义) | 32.7 | 257.2 |
| aligned(对齐窗口,缓冲读) | 32.1 | 241.6 |
| **direct(对齐窗口 + O_DIRECT)** | 173.8 | **188.3** |

设备读阶段耗时(16 个 chunk 并发读的**求和**,ms):

| 读路径 | warm | cold |
| --- | --- | --- |
| legacy | 80.0 | 1584.0 |
| aligned | 97.0 | 1493.0 |
| direct | 1101.0 | 1081.0 |

其他可复现的量化结论:

- **O_DIRECT 命中率**:`direct` 模式下每次请求 16/16 个 chunk 全部命中
  (`X-Compute-Direct-Hits: 16`);`legacy`/`aligned` 为 0/16,与设计一致;
- **对齐开销**:`legacy` 读 67108864 字节,`aligned`/`direct` 读 67115936 字节,
  即每次请求多读 **7072 字节 = 0.0105%**(每 chunk 442 字节);
- 冷缓存下 direct 相对 legacy:端到端中位时延 **−26.8%**,
  设备读阶段求和 **−31.8%**;相对 aligned 分别 −22.1% / −27.6%。

### 5.4 讨论:O_DIRECT 的收益边界

数据同时给出了正反两面的证据:

- **冷数据下 O_DIRECT 更快**,因为它省掉了“磁盘 → 页缓存 → 用户缓冲区”的
  第二次拷贝;这正是近存计算关心的大文件、低缓存命中率场景。
- **热数据下 O_DIRECT 明显更慢**(174ms vs 33ms),因为它不使用页缓存。
  在本容器里页缓存完全装得下 64MB 数据,所以缓冲路径看起来“碾压”O_DIRECT;
  真实负载下工作集远大于页缓存,这种命中率不可持续。
- 因此结论不是“O_DIRECT 一定更快”,而是:**修复让这条路径第一次真正可用**,
  而“何时该用”是一个需要按数据热度/主机内存压力自适应决策的问题——
  这正是阶段二“自适应并发与路径选择”的课题来源。
- 需要说明:本容器为共享环境(无独占 NVMe、有其他负载),时延绝对值仅作趋势参考,
  权威数字需要在 .9 真机上重测。

## 6. 自适应数据通路选择(阶段二前置原型)

第 5.4 节的反向结论直接指向一个设计问题:既然 O_DIRECT 在冷数据上更快、
页缓存命中时更慢,能不能**先感知、再决定**?本节实现了该原型并量化收益。

### 6.1 感知手段与实测

| 手段 | 语义 | 实测 |
| --- | --- | --- |
| `mincore(2)` | mmap 后逐页报告常驻状态 | 命中 100%(2048/2048 页),`fadvise(DONTNEED)` 后 0%;4MiB 探测 66µs(首次含 mmap 开销 247µs) |
| `preadv2(RWF_NOWAIT)` | 不等待地读,**成功即数据已到手** | 命中→立即返回;未命中→`EAGAIN`,10µs |

选 `RWF_NOWAIT` 的理由是「探测即读取」:命中时那次 syscall 本身就是数据搬运,
没有额外开销;未命中只是一次 10µs 的快速失败。相对 30–200ms 的请求时延,
探测成本可忽略,因此**按请求探测是可行的**——这一点与直觉相反。

### 6.2 策略与实现

- agent 新增 `--cache-policy=auto|direct|buffered`(默认 `auto`);
- `auto` 流程:用 `preadv2(RWF_NOWAIT)` 读对齐窗口 → 完整成功即采用缓存结果;
  `EAGAIN` 或**短读(部分命中)** → 丢弃并回退到 O_DIRECT 整窗重读;
- 回退是显式的,**预测失败只会退化为原路径,不会让请求失败**;
- 计数 `cache_hits` / `cache_partial` / `cache_misses` / `probe_errors`,
  经 `X-CSD-Path` → `X-Compute-Cache-Hits` 汇总到 filer 侧。

### 6.3 实验结果

64MB / 16 chunk,9 次重复,中位数:

| 策略 | 热(先经普通读预热页缓存) | 冷(每请求驱逐) | 缓存命中 |
| --- | --- | --- | --- |
| `direct`(始终 O_DIRECT) | 168.5 ms | 197.0 ms | 0/16 |
| `auto`(自适应) | **27.1 ms** | 204.5 ms | 16/16(热)、0/16(冷) |

- 热场景自适应快 **6.2 倍**(27.1 vs 168.5ms),因为走了页缓存;
- 冷场景与纯 O_DIRECT 基本持平(204.5 vs 197.0ms),代价在噪声量级;
- 所有配置结果依然完全一致(`190022035469879755652`),正确性不受策略影响。

### 6.4 一个必须写下来的反面结论:自适应无法自举

第一次实验里 `auto` 的热场景**没有拿到任何收益**(184ms,与 direct 持平)。
原因是:**O_DIRECT 读不填充页缓存**。如果 compute 路径是文件唯一的读者,
缓存永远是冷的,`auto` 会持续命中失败、退化成「永远 direct」。

把热场景改成「先用普通读(模拟 FUSE / S3 / 副本复制)预热」后,收益立刻出现。
由此得到两条设计结论:

1. 自适应策略的收益依赖**数据也被普通读路径访问过**;
2. 若负载是「反复计算同一区间、且只经过 compute 路径」,需要额外机制
   (例如首次访问做一次带缓存的读来「种」缓存,或按热度统计周期性回退到缓冲读),
   而这类机制必须权衡缓存污染,是阶段三值得展开的点。

## 7. 单元与参考测试验证

| 验证项 | 命令 | 结果 |
| --- | --- | --- |
| Go 计算/调度单测 | `go test ./weed/server/ -run 'CSD\|Compute\|Rank\|Scheduler'` | ok,15 个用例 |
| 对齐窗口单测 | `go test ./weed/storage/ -run TestComputeAlignedWindow` | ok |
| 编译 | `go build ./weed/server/ ./weed/storage/` | 通过 |
| C++ 主机侧参考模型 | `make test` | **359 项检查全部通过** |
| agent 类型检查(无需 XRT) | `make syntax-check` | 通过,`-Wall -Wextra` 无告警 |

新增用例要点:窗口对齐/覆盖/最小性;**对齐窗口 == payload 对齐结果的等价性**
(48 组组合);v1/v2 逐位兼容;短读重组与越界短计数;窗口越过 EOF 的判定;
failover 排除集;字节加权与请求数次级信号;以及一个确定性用例证明
能力探测不再被调度锁串行化。

## 8. 交付物

### SeaweedFS 仓库(分支 `feat/csd-native-compute`)

| 文件 | 改动 |
| --- | --- |
| `weed/storage/volume_read.go` | `ComputeAlignedWindow` + 窗口字段 |
| `weed/storage/volume_read_alignment_test.go` | 新增 |
| `weed/server/csd_compute.go` | 请求扩展 + 设备耗时透传 |
| `weed/server/filer_csd_scheduler.go` | 探测移出临界区、排除集、字节加权 |
| `weed/server/filer_csd_scheduler_rank.go` | 新增(候选/排序/负载快照) |
| `weed/server/filer_csd_scheduler_failover_test.go` | 新增 |
| `weed/server/filer_server.go` | `csdInflightBytes` / `csdInflightReqs` |
| `weed/server/filer_server_handlers_proxy.go` | 排除集 failover + 耗时汇总 |
| `csd-experiments/ROADMAP.md` | 后续工作计划 |
| `csd-experiments/tools/csd-agent-sim/` | 新增:无 FPGA 的软件 CSD agent |
| `csd-experiments/scripts/run_phase1_local.sh` | 新增:本地部署实验 |
| `csd-experiments/results/phase1_local.csv` / `_summary.txt` | 新增:三读路径实验数据与汇总 |
| `csd-experiments/results/phase1_cache_policy.csv` / `_summary.txt` | 新增:自适应策略实验数据与汇总 |

### SmartSSD 模块仓库

| 文件 | 改动 |
| --- | --- |
| `src/aligned_range.h` | 新增(窗口几何、EOF 截断、参考求和模型、读满循环) |
| `src/file_sum64_kernel.cpp` | v2 内核 |
| `src/csd_agent.cpp` | 对齐 O_DIRECT、EOF 截断、短读校验、内核版本探测、审计日志、`--cache-policy` 自适应策略 |
| `src/range_sum_test.cpp` | 新增(主机侧参考测试) |
| `tools/cl_stub/` | 新增(仅编译检查用 OpenCL 桩头) |
| `Makefile` / `README.md` | `make test`、`make syntax-check` 与说明 |

## 9. 在 .9 机器上需要执行的收尾步骤

```bash
# 1. 合成 v2 xclbin(约 2 小时)
v++ -t hw --platform <u2_gen3x4_platform> -c -k file_sum64 \
    --temp_dir _x.hw -o _x.hw/file_sum64.xo src/file_sum64_kernel.cpp
v++ -t hw --platform <u2_gen3x4_platform> --link \
    --temp_dir _x.hw -o _x.hw/file_sum64.link.xclbin _x.hw/file_sum64.xo

# 2. 构建并启动 agent,确认识别为 v2 内核
make && ./build/csd_agent --xclbin=file_sum64.xclbin --listen=127.0.0.1:18090

# 3. 用真实 needle 发起计算,检查审计日志
#    期望:kernel=v2 direct=yes,且 direct_hits 持续增长

# 4. 重测性能:冷/热缓存下 legacy vs direct,记录 X-Compute-* 分段耗时
```

验收标准:非对齐 needle 请求日志出现 `direct=yes`;`direct_misses` / `short_reads`
为 0(或可解释);求和结果与 CPU 参考一致。

## 10. 已知环境问题(与本次改动无关)

在本次容器中执行 `go test ./weed/storage/` 时,`TestMemIndexCompaction` 会超时
(卡在 `fsync`)。已用 `git stash` 在**未修改的原始基线**上复跑同一用例,
现象完全一致,属于容器磁盘/IO 压力导致的既有问题。

## 11. 复现命令

```bash
# Go 单元测试
cd seaweedfs_csdsdk
go build ./weed/server/ ./weed/storage/
go test ./weed/server/ -run 'CSD|Compute|Rank|Scheduler' -count=1
go test ./weed/storage/ -run TestComputeAlignedWindow -count=1

# C++ 参考测试与类型检查(无需 XRT)
cd ../smartssd_compute_module && make test && make syntax-check

# 本地部署 + 性能实验(需要先构建 weed 与软件 agent)
cd ../seaweedfs_csdsdk
go build -o /tmp/weed_phase1 ./weed/
go build -o /tmp/csd-agent-sim ./csd-experiments/tools/csd-agent-sim/
WEED=/tmp/weed_phase1 AGENT=/tmp/csd-agent-sim \
  ./csd-experiments/scripts/run_phase1_local.sh 64 9

# 自适应缓存策略对比(热场景会先用普通读预热页缓存)
WEED=/tmp/weed_phase1 AGENT=/tmp/csd-agent-sim MODES="direct auto" \
  CSV=./csd-experiments/results/phase1_cache_policy.csv \
  ./csd-experiments/scripts/run_phase1_local.sh 64 9
```
