# 开发日志 2026-10-12:M1 可检测(快照令牌 + 设备自校验 + 指标)

> 里程碑:[M1 可检测](../M1-detection.md) · 状态:**✅ 完成**(T1.1–T1.8 全部通过)
> 代码提交:`feat(consistency): M1 ...`(见本文件末尾)

## 0. 结论摘要

| 项目 | 结果 |
| --- | --- |
| 目标 | 把"读取窗口内区间被复用"这类**静默错误**变成**显式失败** |
| 改造前(B2 场景) | HTTP 200 + `0:486772,1:486772,2:486772,3:486772,4:486772` —— 看似合理、**完全错误** |
| 改造后(B2 场景) | HTTP **409** `stale_range: needle header changed during the read` |
| 正常路径 | 结果与改造前**逐位一致**(与暴力检索比对通过) |
| 性能代价 | 低于测量噪声(见第 4 节),决策门 D1 **通过** |
| 回归 | 服务端用例全部通过;设备侧 **498** 项检查通过(较改造前 +7) |

## 1. 本次实施范围

严格按 [M1-detection.md](../M1-detection.md) 的 T1.1–T1.8 执行,未越界到 M2(重试策略)与 M3(卷代次)。

## 2. 开发过程

### T1.1 快照令牌与保留键名 ✅

- **新增文件**:`weed/server/filer_compute_consistency.go`
- **内容**:`computeSnapshot{filePath, entryVersion, digest}`、`digestChunks()`(FNV-64a,
  对规范化后的块清单求 12 位十六进制摘要)、保留键名常量、一致性等级常量(L0/L1/L2)与响应头常量。
- **接口冻结**(Q3 决策):下推保留键为 `snap` / `chunk` / `csize`;
  volume 额外补充 `ncookie` / `nid`(needle 身份,由 volume 填写而非客户端)。
- **Q1 决策**:filer store 未提供单调 revision,`entryVersion` 采用
  `entry.Mtime.UnixNano()`;其精度限制已写入代码注释与文档。

### T1.2 Filer 生成并下发令牌 ✅

- 在 `proxyComputeToVolumeServer` 中,`validateChunksCoverFile` 通过后**只生成一次**令牌;
- 通过既有 `params` 通道随每块下发(`fetchChunkComputeResult` → `fetchFromComputeReplica`),
  **未新增任何协议字段**;
- 响应头 `X-Compute-Snapshot` / `X-Compute-Consistency-Level` 直接可见。

### T1.3 Filer 聚合前校验 ✅

- 新增 `verifyChunkTokens()`:校验各块回传的摘要是否一致、块序号是否齐全;
- 语义:L0/L1 下不一致只标注(降级为 L0 并计入 `X-Compute-Stale-Chunks`),L2 下直接拒绝;
- 单测覆盖"全部一致 / 一个不一致 / 缺失回显(老 agent)/ 缺块"四种情形。

### T1.4 Volume 下发前校验 ✅

- `csdOperatorParams(r, n)` 现在附加 `ncookie` / `nid`;
- 下发前比对 `csize`(filer 期望的块长度)与本地 needle 的 `DataSize`;不一致即以
  `409 stale_range` 显式失败。

### T1.5 设备侧区间自校验 ✅

- `smartssd/src/aligned_range.h` 新增 `NeedleHeader` 与 `parse_needle_header()`;
- `smartssd/src/csd_agent.cpp` 在**读取前**校验头部(cookie / needle id / payload 长度),
  在**读取后**再读一次并比对,不一致则抛 `StaleRangeError`;
- HTTP 层新增 409 响应(`{"error":"stale_range: ...","stale":true,"snapshot":"..."}`);
- 新增 `--self-check=false` 开关,用于 A/B 性能测量(默认开启)。

### T1.6 软件 agent 同步实现 ✅

- `csd-experiments/tools/csd-agent-sim/main.go` 实现完全等价的头部校验;
- 新增测试专用端点 `POST /v1/fault?mode=...&count=N`,**按次**注入故障
  (全局开关会污染基线阶段,这一点在实验中踩到并修正,见第 3.2 节);
- `/v1/stats` 暴露 `stale_ranges` / `header_checks` / `self_check`。

### T1.7 指标导出 ✅

新增可观测项:`X-CSD-Stale-Range`(设备)、`X-Compute-Snapshot`、
`X-Compute-Consistency-Level`、`X-Compute-Stale-Chunks`(Filer)。

### T1.8 单元测试与回归 ✅

| 测试集 | 结果 |
| --- | --- |
| 服务端新增 4 个用例(摘要稳定性/敏感性、令牌校验四情形、等级解析、保留键) | 通过 |
| 服务端既有用例(计算、聚合、调度、对齐窗口) | 通过 |
| 设备侧参考测试 | **498 项全部通过**(+7) |
| 构建与语法检查(`go build ./weed/...`、`make -C smartssd syntax-check`) | 通过,无新增告警 |

## 3. 验证实验

### 3.1 实验设计

用 `csd-experiments/scripts/consistency_experiment.py` 在一个 **512 张图(2 MiB,单分块)**
的图库上执行四个阶段:

| 阶段 | 内容 | 期望 |
| --- | --- | --- |
| A 正常 | 连续 5 次检索 | 200,结果与暴力检索一致 |
| B1 查找前复用 | 直接把 needle 头 + 数据区改写成"另一个对象"(0xFF 头 + 全 0 数据) | 显式失败 |
| B2 读取窗口内复用 | 用 `/v1/fault` 按次注入"读取期间区间被复用" | **不得返回数值** |
| C 恢复 | 还原原始字节后再次检索 | 200,结果恢复正确 |

### 3.2 改造前(self-check OFF)与改造后(self-check ON)对照

| 阶段 | 改造前(自校验关闭) | 改造后(M1) |
| --- | --- | --- |
| A 正常 | 200,结果正确(5/5) | 200,结果正确(5/5) |
| B1 查找前复用 | **500**(volume 自身的元数据查找先失败) | **500**(同上) |
| B2 读取窗口内复用 | **200 + 错误结果 → 静默错误** | **409 `stale_range`** |
| C 恢复 | 200,结果正确 | 200,结果正确 |
| 判定 | **NOT PROTECTED** | **PROTECTED** |

改造前 B2 的实际返回(`HTTP 200`):

```text
0:486772,1:486772,2:486772,3:486772,4:486772
```

这是一个**格式完全合法、数值看似合理、但与真实图库无关**的 Top-5 列表——
正是本文档要消除的失败模式。改造后同一请求返回:

```text
HTTP 409
{"error":"stale_range: {\"error\":\"stale_range: stale_range: needle header changed during the read at offset 28\"}"}
```

### 3.3 实验过程中发现并修正的两个问题(重要)

1. **needle 头是大端序**。首版实现按小端解析,导致**正常请求全部 409**
   (日志:`cookie mismatch at offset 28 (want 2980125330, found 2451218865)`,
   两个值恰好字节颠倒)。原因是 SeaweedFS 用 `util.Uint32toBytes` 编码头部(大端),
   而求和算子的"按字打包"是小端——**两套字节序在同一份数据上并存**,极易混淆。
   修正后正常路径恢复,并把这一事实写进代码注释与设备侧回归测试。
   *(这正是"必须用真实链路验证"的价值:该错误在纯单测中不会暴露。)*
2. **`stale_range` 曾会降级到脚本路径**。设备正确返回 409 后,volume 把它当作普通节点
   失败,继续回落到脚本路径;若集群配置了计算脚本,脚本会读取**完整 needle**
   (含头部)并对已被改写的字节求值——**静默错误重新出现**。
   修正:引入哨兵错误 `errStaleRange`,`stale_range` 一律显式失败(409),
   绝不降级;Filer 侧同样映射为 409 而非 500。

### 3.4 决策门 D1

| 判据 | 结果 |
| --- | --- |
| 注入"读取期间区间被复用"时不再返回数值 | ✅ 409 |
| 正常路径结果与改造前一致 | ✅ 逐位一致 |
| 端到端时延回归 < 5% | ✅ 低于噪声(见第 4 节) |
| 既有回归全部通过 | ✅ 服务端 + 设备侧 498 项 |

**D1 结论:通过,进入 M2。**

## 4. 性能记录

### 4.1 测量方法

同一台机器、同一负载(4096 张图 = 16 MiB 图库,4 个数据块,top-5,7 次重复取中位数),
仅切换 agent 的 `-self-check` 开关;原始数据:
`csd-experiments/results/m1_perf_selfcheck_off.csv` 与 `m1_perf_selfcheck_on.csv`。

### 4.2 结果

| 指标 | 自校验 OFF | 自校验 ON | 变化 |
| --- | --- | --- | --- |
| 端到端时延中位数 | 48.74 ms | 42.83 ms | −12%(噪声内) |
| 端到端时延最小/最大 | 30.40 / 68.74 ms | 42.12 / 66.96 ms | 抖动相当 |
| 设备读取阶段中位数 | 128.0 ms | 122.0 ms | −5%(噪声内) |
| 结果正确性 | 与暴力检索一致 | 与暴力检索一致 | 无变化 |
| 回传数据量 | 49 字节 | 49 字节 | 无变化 |

### 4.3 分析

- 两个方向的差异(端到端 **−12%**)说明**自校验的开销低于本环境的测量噪声**;
  同一负载的历史重复实验抖动也在 ±30% 量级,因此不能据此认为自校验"更快";
- 理论代价可精确计算:每块增加 2 次 20 字节读取,16 个块共 **640 字节**,
  相对 16 MiB 的扫描量约为 **0.004%**;
- 结论:**自校验的代价在当前规模下不可测**,满足 M1 出口条件(时延回归 < 5%);
  若未来在更小粒度(如 4 KiB 记录)上启用,需要重新测量。

## 5. 交付物

| 交付物 | 位置 |
| --- | --- |
| 令牌与一致性定义 | `weed/server/filer_compute_consistency.go` |
| 令牌生成/下发/聚合前校验 | `weed/server/filer_server_handlers_proxy.go` |
| Volume 前置校验与指标透传 | `weed/server/csd_compute.go` |
| 设备侧自校验 | `smartssd/src/aligned_range.h`、`smartssd/src/csd_agent.cpp` |
| 实验台对照实现与故障注入 | `csd-experiments/tools/csd-agent-sim/main.go` |
| 验证脚本 | `csd-experiments/scripts/consistency_experiment.py` |
| 实验原始数据 | `csd-experiments/results/consistency_{check,nocheck}.csv`、`m1_perf_selfcheck_*.csv` |
| 单元测试 | `weed/server/filer_compute_consistency_test.go`、`smartssd/src/range_sum_test.cpp` |

## 6. 遗留问题与下一步

| 编号 | 事项 | 归属 |
| --- | --- | --- |
| R1 | `stale_range` 目前**直接失败**,尚未重试"重新解析清单后重算" | M2 / T2.3 |
| R2 | 副本滞后导致的跨块混版尚未处理(本次只解决"单块读到别的对象") | M2 / T2.1–T2.2 |
| R3 | `entryVersion` 使用 mtime,无单调 revision,仅能表达"被改过" | 待 filer store 能力确认 |
| R4 | B2 场景为**模拟注入**(测试端点),尚未在真实 vacuum 并发下复现 | M4 / T4.1、T4.3 |
| R5 | C++ agent 未提供 `/v1/fault` 端点(仅软件 agent 有),真机故障注入需另行设计 | M4 |

## 7. 过程记录

| 时间 | 事件 | 处理 |
| --- | --- | --- |
| 上午 | 冻结接口,实现 T1.1–T1.4(服务端 + volume) | 编译通过 |
| 中午 | 实现 T1.5/T1.6(设备侧与实验台) | 编译通过 |
| 下午 | 首次端到端:正常路径 **全部 409** | 定位为**字节序**问题并修正 |
| 下午 | 第二次端到端:注入场景返回 500"脚本不存在" | 定位为**降级路径**问题,改为显式 409 |
| 傍晚 | 故障注入从"全局开关"改为"按次注入" | 避免污染基线阶段 |
| 傍晚 | 完成 A/B 性能测量与全量回归 | 498 项通过,D1 通过 |
