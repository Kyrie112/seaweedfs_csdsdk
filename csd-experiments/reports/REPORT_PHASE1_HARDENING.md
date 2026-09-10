# 阶段一:P2P 对齐通路与调度加固 —— 实施与验证报告

本报告对应 [ROADMAP.md](../ROADMAP.md) 的阶段一工作:修复三个已定位的真实缺陷,
并补齐分阶段耗时观测。所有改动均已提交,可在无 FPGA 的环境下复现验证。

## 1. 结论摘要

| 工作项 | 状态 | 结论 |
| --- | --- | --- |
| P1-1 对齐读通路(含内核 v2) | 已实现,语义已在主机侧证明 | 真实 needle 请求从此可走 O_DIRECT,且结果与修复前逐字节一致 |
| P1-2 短读/部分读校验 | 已完成 | 短读不再被当作成功,循环读满并显式报错 |
| P1-3 调度器加固 | 已完成 | 探测不再持锁、失败副本被排除、负载按字节加权 |
| P1-4 分阶段耗时观测 | 已完成 | dispatch / 设备读 / 内核三段耗时可按 chunk 导出 |
| 真机 xclbin 重合成与端到端实测 | **待执行**(需 .9 机器) | 命令与验收标准见第 6 节 |

关键结论用一句话概括:**“P2P + O_DIRECT”在过去从未对真实业务请求生效**,
而修复它的前提是设备侧内核必须能跳过对齐窗口的头部偏移——这一点已用
354 项主机侧检查证明为数值等价。

## 2. 背景:三个已定位的缺陷

| 缺陷 | 影响 | 位置(修复前) |
| --- | --- | --- |
| `O_DIRECT` 判定恒为假 | 近存计算的“零拷贝”卖点未兑现,实际走 page cache + 主机内存 | `smartssd_compute_module/src/csd_agent.cpp:198` |
| 短读未校验 | 读到部分数据时静默返回错误结果 | `csd_agent.cpp:206` |
| 调度器锁粒度过大 / 重试无排除集 / 负载按请求计数 | 并发扇出被网络探测串行化;failover 可能重选失败副本;大 chunk 与小 chunk 权重相同 | `weed/server/filer_csd_scheduler.go` |

### 2.1 对齐缺陷的根因

SeaweedFS needle 的数据区起点是

```text
DataOffset = actualOffset + NeedleHeaderSize + DataSizeSize = actualOffset + 20
```

而 needle 在 `.dat` 中没有填充对齐,`actualOffset` 本身也不保证对齐。
因此 `offset % 512 == 0 && size % 512 == 0` 这个判定对真实业务请求几乎恒为假。
此前 README 中记录的 “512B O_DIRECT + P2P:期望 1760,返回 1760” 之所以成立,
是因为该用例是**人工构造**的对齐请求,不代表真实 needle 布局。

## 3. 修复设计

### 3.1 P1-1 对齐读通路

**为什么不能只在主机侧“裁掉”窗口头部的多余字节。**

内核把缓冲区按小端 `uint64` 打包求和,其数值等于

```text
S = Σ_k b_k · 256^(k mod 8)      (k 为相对缓冲区起始的字节下标)
```

若缓冲区起点比 payload 起点提前 `δ = payload_shift` 字节,则 payload 第 `j` 字节落在
窗口下标 `δ + j` 处,直接求和得到的是 `Σ_j b_j · 256^((δ+j) mod 8)`,
与期望的 `Σ_j b_j · 256^(j mod 8)` 不同——**只要 `δ mod 8 ≠ 0`,每个 payload 字节的
位权都变了**,不是减去一个前缀和就能修正的。真实 needle 布局恰好落在这种情况:
`needleOffset = 0` 时 `δ = 20`,`20 mod 8 = 4 ≠ 0`。

因此只有两条路:(a) 主机侧再多做一次整段搬运把 payload 对齐到缓冲区首址——
这等于放弃“无主机拷贝”;或 (b) 把偏移交给设备。本工作选择 (b)。

**协议扩展。** volume 侧在区间描述符中新增对齐窗口:

```json
{
  "operation": "rawsum64",
  "data_file": "/var/lib/seaweedfs/2.dat",
  "offset": 255348, "size": 8388608,
  "read_offset": 254976, "read_size": 8389120, "payload_shift": 372
}
```

窗口按 `512` 字节对齐且最小覆盖 payload;旧 agent 忽略未知字段后仍按原语义工作。

**内核 v2。** `file_sum64` 新增 `payload_shift` / `payload_size` 两个参数,
打包从 payload 首字节起算:

```c
word |= (uint64_t)file_data[payload_shift + payload_index] << (j << 3);
```

当 `payload_shift = 0`、`payload_size = byte_count` 时与 v1 逐位等价。

**兼容策略。** agent 启动时用 `CL_KERNEL_NUM_ARGS` 探测:3 参数按 v1 语义
(缓冲区从 payload 开始,无法 O_DIRECT),5 参数按 v2 语义;其它参数个数直接报错,
避免“用错参数算出错误结果”。

### 3.2 P1-2 读取完整性

`read_fully_with` 循环读取直到读满,遇到 EOF 返回短计数;调用方比较
`got != byte_count` 即报错并计入 `short_reads`。这样 `O_DIRECT` 的整块语义
和普通 `pread` 的短读都能被同一段逻辑覆盖。

### 3.3 P1-3 调度器加固

| 问题 | 修复 |
| --- | --- |
| `csdSchedMu` 覆盖网络探测 | 探测(`probeReplicaCapabilities`)在锁外完成,临界区只做“排序内存快照 + 预留” |
| 重试可能选回失败副本 | 新增排除集,failover 每次保证命中未失败副本;全部排除时显式报错 |
| 负载按请求数计 | 主信号改为在途字节 `csdInflightBytes`,请求数 `csdInflightReqs` 降为次级信号 |

排序目标变为:`CSD 能力(硬约束) → 在途字节 → 在途请求数 → 探测延迟 → host 字典序`。

### 3.4 P1-4 分阶段耗时

agent 返回 `read_ms` / `kernel_ms` / `direct`;volume 侧转成
`X-CSD-Direct` / `X-CSD-Read-Ms` / `X-CSD-Kernel-Ms` 响应头;
filer 侧按 chunk 记录 `dispatch_ms`,并在整请求上汇总为
`X-Compute-Chunks` / `X-Compute-Dispatch-Ms` / `X-Compute-Device-Read-Ms` /
`X-Compute-Kernel-Ms` / `X-Compute-Direct-Hits` / `X-Compute-Payload-Bytes`。
这些字段可直接用于绘制阶段耗时瀑布图。

## 4. 验证结果

### 4.1 Go 侧

| 验证项 | 命令 | 结果 |
| --- | --- | --- |
| 计算/调度相关单测 | `go test ./weed/server/ -run 'CSD\|Compute\|Rank\|Scheduler' -count=1` | `ok`(19.3s,15 个用例全部通过) |
| 对齐窗口单测 | `go test ./weed/storage/ -run TestComputeAlignedWindow` | `ok` |
| 编译 | `go build ./weed/server/ ./weed/storage/` | 通过 |

新增用例:

- `TestComputeAlignedWindowProperties`:窗口对齐、覆盖、最小性、shift 范围(7 组子用例);
- `TestComputeAlignedWindowKeepsPayloadPackable`:真实 needle 布局下
  `read_offset + payload_shift == data_offset` 恒成立;
- `TestRankAndReserveSkipsExcludedReplicas` / `...FailsWhenAllReplicasExcluded`:failover 排除集;
- `TestRankComputeReplicasPrefersLowerByteLoad` / `...UsesRequestCountAsTieBreaker`:字节加权与次级信号;
- `TestRankAndReserveReleasesByteLoad`:预留/释放与重复释放的计数正确性;
- `TestRankAndReserveDoesNotSerializeCapabilityProbes`:确定性回归用例——
  状态探测被门控阻塞,若实现仍在持锁状态下探测,第二个调用无法进入探测,
  用例会立刻失败而不是挂死。

### 4.2 C++ 侧(主机侧参考模型)

| 验证项 | 命令 | 结果 |
| --- | --- | --- |
| 参考模型与语义等价性 | `make test` | `range_sum_test: all 354 checks passed` |
| agent 类型检查(无需 XRT) | `make syntax-check` | 通过(`-Wall -Wextra` 无告警) |
| 内核源码检查 | `g++ -fsyntax-only -std=c++17 -Wno-unknown-pragmas src/file_sum64_kernel.cpp` | 通过 |

`range_sum_test` 覆盖六类检查:

1. 窗口几何:对齐、覆盖、最小、`0 ≤ shift < 512`(6 种 needle 偏移 × 8 种 payload 长度);
2. **语义等价性**:读对齐窗口 + 按 `payload_shift` 打包 == 直接从 payload 打包(48 组组合);
3. v1 兼容:`payload_shift=0, payload_size=byte_count` 与 v1 模型逐位一致(含非 8 字节倍数长度);
4. **缺陷证据**:直接对对齐窗口求和会得到不同的值(真实布局 `δ=20`),即内核 v2 存在的必要性;
5. 短读:分片 100 字节读取被正确重组;越过 EOF 时返回短计数而非静默成功;
6. 真实文件描述符上的 `read_fully_at` 行为一致。

### 4.3 尚未完成的验证

以下需要 SmartSSD 实机(`192.168.0.9`),当前容器没有 XRT/FPGA,无法执行:

- 用 v2 内核重新合成 xclbin;
- 端到端请求验证 `direct=yes` 且 `direct_hits` 增长(即 O_DIRECT 真正生效);
- 修复前后端到端时延与主机内存流量对比(论文用图)。

## 5. 交付物

### SeaweedFS 仓库(`feat/csd-native-compute`,提交 `62c977c4c`)

| 文件 | 改动 |
| --- | --- |
| `weed/storage/volume_read.go` | `ComputeAlignedWindow` + 窗口字段 |
| `weed/storage/volume_read_alignment_test.go` | 新增 |
| `weed/server/csd_compute.go` | 请求扩展 + 设备耗时透传 |
| `weed/server/filer_csd_scheduler.go` | 探测移出临界区、排除集、字节加权 |
| `weed/server/filer_csd_scheduler_rank.go` | 新增(候选/排序/负载快照) |
| `weed/server/filer_csd_scheduler_failover_test.go` | 新增 |
| `weed/server/filer_server.go` | `csdInflightBytes` / `csdInflightReqs` |
| `weed/server/filer_server_handlers_proxy.go` | 排除集 failover + 耗时汇总与响应头 |
| `csd-experiments/ROADMAP.md` | 后续工作计划 |

### SmartSSD 模块仓库(提交 `ba54fd7`)

| 文件 | 改动 |
| --- | --- |
| `src/aligned_range.h` | 新增(窗口几何、参考求和模型、读满循环) |
| `src/file_sum64_kernel.cpp` | v2 内核 |
| `src/csd_agent.cpp` | 对齐 O_DIRECT、短读校验、内核版本探测、审计日志与耗时 |
| `src/range_sum_test.cpp` | 新增(主机侧参考测试) |
| `tools/cl_stub/` | 新增(仅编译检查用 OpenCL 桩头) |
| `Makefile` / `README.md` | `make test`、`make syntax-check` 与说明 |

## 6. 在 .9 机器上需要执行的收尾步骤

```bash
# 1. 合成 v2 xclbin(约 2 小时)
v++ -t hw --platform <u2_gen3x4_platform> -c -k file_sum64 \
    --temp_dir _x.hw -o _x.hw/file_sum64.xo src/file_sum64_kernel.cpp
v++ -t hw --platform <u2_gen3x4_platform> --link \
    --temp_dir _x.hw -o _x.hw/file_sum64.link.xclbin _x.hw/file_sum64.xo

# 2. 构建并启动 agent,确认内核版本被识别为 v2
make && ./build/csd_agent --xclbin=file_sum64.xclbin --listen=127.0.0.1:18090

# 3. 对真实 needle 发起计算请求,检查审计日志
#    期望:kernel=v2 direct=yes,且 direct_hits 持续增长

# 4. 对比修复前后:P2P 端到端时延、主机内存流量、X-Compute-* 分段耗时
```

验收标准:

- 非对齐 needle 请求的日志出现 `direct=yes`,`direct_misses` 不随请求增长;
- 求和结果与 CPU 参考实现一致(与 `rawsum64` 语义一致);
- `direct_misses`/`short_reads` 计数为 0(或可解释)。

## 7. 已知环境问题(与本次改动无关)

在本次容器中执行 `go test ./weed/storage/` 时,`TestMemIndexCompaction` 会超时
(卡在 `fsync`)。已用 `git stash` 在**未修改的原始基线**上复跑同一用例,
现象完全一致,因此这是容器磁盘/IO 压力导致的既有问题,不是本次改动引入。
与本次改动直接相关的 `TestComputeAlignedWindow` 及 `weed/server` 全部用例均通过。

## 8. 复现命令

```bash
# Go
cd seaweedfs_csdsdk
go build ./weed/server/ ./weed/storage/
go test ./weed/server/ -run 'CSD|Compute|Rank|Scheduler' -count=1
go test ./weed/storage/ -run TestComputeAlignedWindow -count=1

# C++(无需 XRT / FPGA)
cd ../smartssd_compute_module
make test
make syntax-check
```
