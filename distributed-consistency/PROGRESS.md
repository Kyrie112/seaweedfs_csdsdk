# 进度看板

> 状态图例见 [README](README.md)。本文件是状态的**唯一来源**,其它文档只写过程与证据。

## 里程碑总览

| 里程碑 | 目标 | 步骤数 | 已完成 | 状态 | 出口条件 |
| --- | --- | --- | --- | --- | --- |
| [M1 可检测](M1-detection.md) | 把静默错误变为显式失败 | 8 | 8 | ✅ 已完成 | 注入"读取期间替换 needle"时不再返回数值(实测 409);时延回归 < 5%(低于噪声) |
| [M2 可规避](M2-mitigation.md) | 副本一致性 + 失败语义 | 6 | 0 | ⬜ 未开始 | 副本滞后时 L2 不返回混版结果;错误可分类观测 |
| [M3 可互斥](M3-exclusion.md) | 卷数据文件代次 / 读租约(可选) | 4 | 0 | ⬜ 未开始 | 失效在下发前被发现;整理吞吐回归可接受 |
| [M4 可验证](M4-verification.md) | 故障注入矩阵 + 不变量检查 | 5 | 0 | ⬜ 未开始 | F1–F7 每行有结论;静默错误数为 0 |

## 步骤清单

### M1 可检测

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 | 证据 |
| --- | --- | --- | --- | --- | --- | --- |
| T1.1 | 定义快照令牌与保留键名 | `weed/server/filer_compute_consistency.go`(新建) | 开放问题 Q1 | ✅ | 结构体与常量定稿,含字段语义注释 | `filer_compute_consistency.go` 已提交;摘要/等级/保留键单测通过 |
| T1.2 | Filer 生成令牌 | `filer_server_handlers_proxy.go` | T1.1 | ✅ | 每次请求只生成一次,日志可见 | 令牌每请求生成一次,`X-Compute-Snapshot` 可见([日志](journal/2026-10-12-m1-detection.md)) |
| T1.3 | Filer 聚合前校验 | `filer_server_handlers_proxy.go` | T1.2 | ✅ | 序号不齐/快照不一致时显式失败 | `verifyChunkTokens` 四类情形单测通过 |
| T1.4 | Volume 下发前校验 | `weed/server/csd_compute.go` | T1.1 | ✅ | 不匹配返回 `stale_range` | `csize`/`ncookie`/`nid` 校验;不匹配返回 409 |
| T1.5 | 设备侧区间自校验 | `smartssd/src/csd_agent.cpp`、`aligned_range.h` | T1.1 | ✅ | 读取前后头部不一致时拒绝返回结果 | 读取前后头校验;C++ 编译与 498 项检查通过 |
| T1.6 | 软件 agent 同步实现 | `csd-experiments/tools/csd-agent-sim/main.go` | T1.5 | ✅ | 实验台可复现一致性问题 | 软件 agent 等价实现 + `/v1/fault` 按次注入 |
| T1.7 | 一致性指标导出 | `filer_server_handlers_proxy.go`、`csd_compute.go` | T1.3、T1.4 | ✅ | 4 个新指标可在响应头观测 | 4 个指标可在响应头观测 |
| T1.8 | 单元测试(正反用例) | `weed/server/*_test.go`、`smartssd/src/range_sum_test.cpp` | T1.1–T1.7 | ✅ | 新增用例覆盖 4 类失效;既有 491+ 项全过 | 服务端新增 4 用例;设备侧 498 项通过 |

### M2 可规避

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 |
| --- | --- | --- | --- | --- | --- |
| T2.1 | 请求级副本亲和 | `filer_csd_scheduler.go`、`filer_csd_scheduler_rank.go` | M1 完成 | ⬜ | 同一请求副本集合收敛度可测 |
| T2.2 | 副本版本回传与校验 | `filer_compute_aggregation.go` | T2.1 | ⬜ | L2 下混版请求被拒绝或重试 |
| T2.3 | `stale_range` 重试策略 | `filer_server_handlers_proxy.go` | T1.4 | ⬜ | 重新解析清单并重试一次,不活锁 |
| T2.4 | 错误分类与响应表达 | `csd_compute.go`、`filer_server_handlers_proxy.go` | T2.3 | ⬜ | 四类错误码可从响应头区分 |
| T2.5 | 一致性等级 API | `filer_compute_api.go`、`filer_server_handlers_read.go` | T2.2 | ⬜ | `consistency=` / `on_stale=` 生效且有文档 |
| T2.6 | 一致性单元测试 | `weed/server/*_test.go` | T2.1–T2.5 | ⬜ | 构造滞后副本可稳定复现并检出 |

### M3 可互斥(可选)

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 |
| --- | --- | --- | --- | --- | --- |
| T3.1 | 卷数据文件代次 | `weed/storage/volume.go`、`volume_vacuum.go` | M1 数据表明需要 | ⬜ | 代次在替换临界区内单调递增 |
| T3.2 | 代次随描述符下发与校验 | `csd_compute.go`、`volume_read.go` | T3.1 | ⬜ | 代次不匹配在下发前失败 |
| T3.3 | 读租约(可选) | `weed/storage/volume.go` | T3.2 | ⬜ | 计算期间整理被推迟且可观测 |
| T3.4 | 并发压力回归 | 存储层测试 | T3.1–T3.3 | ⬜ | 整理吞吐回归在可接受范围 |

### M4 可验证

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 |
| --- | --- | --- | --- | --- | --- |
| T4.1 | 故障注入工具 | `csd-experiments/scripts/`(新增) | M1、M2 | ⬜ | 可脚本化触发 F1–F7 |
| T4.2 | 不变量检查器 | 同上 | T4.1 | ⬜ | 能自动判定"结果属于某个快照" |
| T4.3 | 执行 F1–F7 矩阵 | 同上 | T4.1、T4.2 | ⬜ | 每行有明确结论 |
| T4.4 | 一致性报告 | `csd-experiments/reports/`(新增) | T4.3 | ⬜ | 含代价数据与结论 |
| T4.5 | 回填论文 | `THESIS_DRAFT.md` | T4.4 | ⬜ | 第 4/6 章补充一致性与实验 |

## 阻塞与待确认

| ID | 事项 | 影响步骤 | 状态 |
| --- | --- | --- | --- |
| Q1 | filer store 是否提供单调 revision(否则用 mtime) | T1.1 | ✅ 已决策:用 `Mtime.UnixNano()` |
| Q2 | 设备侧校验头部是否需要额外 fd | T1.5 | ✅ 已决策:复用 buffered fd |
| Q3 | `params` 保留键名约定 | T1.1、T1.4 | ✅ 已决策:`snap`/`chunk`/`csize`/`ncookie`/`nid` |
| Q4 | L2 重试是否会放大负载(与扇出/每主机上限联动) | T2.3 | ⬜ 待确认 |
| Q5 | 降级到脚本路径时是否重新校验版本 | T2.4 | ⬜ 待确认 |

## 里程碑决策门

> M1 决策门 D1 已于 2026-10-12 通过,详见
> [journal/2026-10-12-m1-detection.md](journal/2026-10-12-m1-detection.md)。

| 决策门 | 位置 | 判据 |
| --- | --- | --- |
| D1 | M1 结束 | ✅ **通过**(2026-10-12):自校验开销低于噪声下限(理论 0.004%),进入 M2 |
| D2 | M2 结束 | 若请求级亲和已能覆盖混版风险,M3 可标记为 ➖ 不适用 |
| D3 | M3 结束 | 整理吞吐回归若超过既定预算,回退读租约,只保留代次校验 |
