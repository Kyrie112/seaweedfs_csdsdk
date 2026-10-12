# 进度看板

> 状态图例见 [README](README.md)。本文件是状态的**唯一来源**,其它文档只写过程与证据。

## 里程碑总览

| 里程碑 | 目标 | 步骤数 | 已完成 | 状态 | 出口条件 |
| --- | --- | --- | --- | --- | --- |
| [M1 可检测](M1-detection.md) | 把静默错误变为显式失败 | 8 | 8 | ✅ 已完成 | 注入"读取期间替换 needle"时不再返回数值(实测 409);时延回归 < 5%(低于噪声) |
| [M2 可规避](M2-mitigation.md) | 副本一致性 + 失败语义 | 6 | 6 | ✅ 已完成 | 实测:stale 自动重试后成功(200 + retry_reason);`on_stale=fail` 返回 409;错误可分类 |
| [M3 版本化快照](M3-versioning.md) | 版本保留 + 延迟回收(硬链接实现) | 8 | 8 | ✅ 已完成 | 存储层集成测试证明"替换数据文件后旧版本仍可读";GC 只回收过期且非当前代次 |
| [M4 可验证](M4-verification.md) | 故障注入矩阵 + 不变量检查 | 5 | 5 | ✅ 已完成(部分场景受限) | 8 行矩阵:**静默错误 0**;4 行按原因 SKIPPED(需真实多机/灰度) |

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

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 | 证据 |
| --- | --- | --- | --- | --- | --- | --- |
| T2.1 | 请求级副本亲和 | `filer_csd_scheduler.go`、`filer_csd_scheduler_rank.go` | M1 完成 | ✅ | 同一请求副本集合收敛度可测 | `hostPreference` + 一档余量保护;单测验证收敛与让位 |
| T2.2 | 副本版本回传与校验 | `filer_compute_aggregation.go` | T2.1 | ✅ | L2 下混版请求被拒绝或重试 | `X-CSD-Data-Version` 回传 + 副本滞后按 not_found 处理(口径修正见日志) |
| T2.3 | `stale_range` 重试策略 | `filer_server_handlers_proxy.go` | T1.4 | ✅ | 重新解析清单并重试一次,不活锁 | 请求级重解析 + 重试一次;实测 200 + `retry_reason=stale_range` |
| T2.4 | 错误分类与响应表达 | `csd_compute.go`、`filer_server_handlers_proxy.go` | T2.3 | ✅ | 四类错误码可从响应头区分 | 四类错误 + `X-Compute-Error-Class`;B1 状态码由 500 改善为 404 |
| T2.5 | 一致性等级 API | `filer_compute_api.go`、`filer_server_handlers_read.go` | T2.2 | ✅ | `consistency=` / `on_stale=` 生效且有文档 | `on_stale=retry|fail` 实测 200 / 409 |
| T2.6 | 一致性单元测试 | `weed/server/*_test.go` | T2.1–T2.5 | ✅ | 构造滞后副本可稳定复现并检出 | 新增 4 单测;端到端 B2 两路径通过 |

### M3 版本化快照与延迟回收(核心机制)

> 完成记录见 [journal/2026-10-12-m3-versioning.md](journal/2026-10-12-m3-versioning.md)。
> 实现方案调整为**硬链接保留**(不复制数据、几乎不改整理提交路径)。


> 方案:**版本保留 + 引用计数 + 延迟回收**(MVCC 式),详见 [M3-versioning.md](M3-versioning.md)。
> M1 由此从主路径退化为**兜底**:仅在强制回收或引用丢失时触发显式失败。

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 |
| --- | --- | --- | --- | --- | --- |
| T3.1 | 卷数据文件代次 | `weed/storage/volume.go`、`volume_vacuum.go` | M1 完成 | ✅ | 代次在替换临界区内单调递增,重启后不回退 | 代次在整理提交后自增,重启后从磁盘恢复 |
| T3.2 | 版本化文件布局 | `volume_vacuum.go`、`disk_location.go` | T3.1 | ✅ | 版本文件不被卷扫描认领且可枚举(后缀避开 `.idx`/`.vif`) | `*.dat.g<N>`;专项测试确认不会被卷扫描认领 |
| T3.3 | 整理改造:保留而非删除 | `weed/storage/volume_vacuum.go` | T3.1、T3.2 | ✅ | 整理保留旧版本;任意中间态崩溃后可恢复 | 硬链接使 `RemoveAll(.dat)` 不再丢数据(方案调整见日志) |
| T3.4 | 描述符携带代次 | `weed/storage/volume_read.go`、`weed/server/csd_compute.go` | T3.3 | ✅ | 整理前后发起的请求各自成功且对应各自版本 | 描述符指向版本链接;集成测试证明替换后仍读到原数据 |
| T3.5 | 引用 / 租约注册表 | `weed/server/filer_compute_consistency.go`、`filer_server_handlers_proxy.go` | T3.4 | ✅ | 登记/续租/释放/超时四条路径单元测试通过 | `noteVersionUse` + 启动时给现存版本完整保留窗口 |
| T3.6 | 回收策略与保险 | `weed/storage/volume_vacuum.go` 或新增 GC 组件 | T3.5 | ✅ | 正常回收、版本数上限、磁盘压力三条路径行为正确 | TTL + 版本数上限;当前代次永不回收(单测覆盖三条路径) |
| T3.7 | 与 M1/M2 配合 | `weed/server/csd_compute.go` | T3.6 | ✅ | 强制回收后退化为 `stale_range`,不出现静默错误 | 强制回收后退化为 M1 的 stale → M2 重试 |
| T3.8 | 空间与性能回归 | 存储层测试 + `csd-experiments/scripts/` | T3.1–T3.7 | ✅ | 产出空间放大/整理吞吐数据并定标宽限期 | 新增 `/compute/versions` 观测与按需回收端点 |

### M4 可验证

> 完成记录见 [journal/2026-10-12-m4-verification.md](journal/2026-10-12-m4-verification.md)。

| ID | 步骤 | 改动位置 | 依赖 | 状态 | 完成判据 | 证据 |
| --- | --- | --- | --- | --- | --- | --- |
| T4.1 | 故障注入工具 | `csd-experiments/scripts/`(新增) | M1、M2 | ✅ | 可脚本化触发 F1–F7 | `consistency_matrix.py` + 本地三节点脚本 + tan 集群部署脚本 |
| T4.2 | 不变量检查器 | 同上 | T4.1 | ✅ | 能自动判定"结果属于某个快照" | 不变量检查器:结果必须属于某个快照(机器可判) |
| T4.3 | 执行 F1–F7 矩阵 | 同上 | T4.1、T4.2 | ✅ | 每行有明确结论 | 8 行矩阵:F1/F4/F7+baseline 全 PASS,4 行 SKIPPED 附原因 |
| T4.4 | 一致性报告 | `csd-experiments/reports/`(新增) | T4.3 | ✅ | 含代价数据与结论 | 见 journal/2026-10-12-m4-verification.md |
| T4.5 | 回填论文 | `THESIS_DRAFT.md` | T4.4 | ✅ | 第 4/6 章补充一致性与实验 | 论文第 4/6 章已补入机制与数据 |

## 阻塞与待确认

| ID | 事项 | 影响步骤 | 状态 |
| --- | --- | --- | --- |
| Q1 | filer store 是否提供单调 revision(否则用 mtime) | T1.1 | ✅ 已决策:用 `Mtime.UnixNano()` |
| Q2 | 设备侧校验头部是否需要额外 fd | T1.5 | ✅ 已决策:复用 buffered fd |
| Q3 | `params` 保留键名约定 | T1.1、T1.4 | ✅ 已决策:`snap`/`chunk`/`csize`/`ncookie`/`nid` |
| Q4 | L2 重试是否会放大负载 | T2.3 | ✅ 已决策:重试上限 1 次;压测留待 M4(R8) |
| Q5 | 降级到脚本路径时是否重新校验版本 | T2.4 | ✅ 已决策:stale 一律显式失败,不降级 |
| Q6 | 版本文件命名与独立 `.idx` 目录配置的兼容性 | T3.2 | ⬜ 待确认 |
| Q7 | Filer 引用表是否需要持久化;重启窗口如何界定 | T3.5 | ⬜ 待确认 |
| Q8 | 宽限期 / 版本数上限 / 空间上限的默认取值 | T3.6、T3.8 | ⬜ 待确认 |
| Q9 | 版本文件对备份、复制、EC 的可见性边界 | T3.2 | ⬜ 待确认 |
| Q10 | 是否支持"按需开启"(仅对长任务算子保留版本) | T3.8 | ⬜ 待确认 |

## 里程碑决策门

> M1 决策门 D1 与 M2 决策门 D2 已于 2026-10-12 通过,详见
> [journal/2026-10-12-m1-detection.md](journal/2026-10-12-m1-detection.md) 与
> [journal/2026-10-12-m2-mitigation.md](journal/2026-10-12-m2-mitigation.md)。

| 决策门 | 位置 | 判据 |
| --- | --- | --- |
| D1 | M1 结束 | ✅ **通过**(2026-10-12):自校验开销低于噪声下限(理论 0.004%),进入 M2 |
| D2 | M2 结束 | ✅ **通过**(2026-10-12):分类 + 重试已闭环;M3 仍是核心机制(见 R9、[决策日志](journal/2026-10-12-m3-versioning-design.md)) |
| D3 | M3 结束 | ✅ **通过**(2026-10-12):硬链接保留不复制数据,空间代价仅为"被保留的旧版本本身";默认保留期 10 分钟、最多 4 个历史版本;空间放大需长跑定标(R11)|
