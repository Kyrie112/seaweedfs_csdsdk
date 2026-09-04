# V4:CSD-aware 副本选择策略(分层优化 + 负载感知 + 自动重试)

## 1. 目的

设计一套可复现、可论证的计算副本调度策略,使每个 chunk 的计算
“只要存在可用的 CSD 副本,就必然落到最优 CSD 副本上”;
不存在 CSD 副本时,则落到预测代价最小的普通 volume server。

## 2. 问题定义

对文件被切分出的 chunk 集合 C,每个 chunk j 存在副本集合 R_j。
调度器为每个 chunk 选择执行服务器 a_j ∈ R_j。

记:

- `C_i` = 服务器 i 是否具备 CSD 能力(硬属性);
- `Q_i` = filer 观察到的服务器 i 在途计算数(软负载);
- `L_i` = `/compute/status` 探测延迟;
- `H_i` = 服务器 host(确定性平局键)。

目标:在满足副本约束与 CSD 能力偏好的前提下,
最小化扇出批次的最差/平均完成时间;同一时刻避免请求堆到同一节点。

## 3. 分层(lexicographic)目标函数

调度的排序键为:

```text
minimize ( -C_i, Q_i, L_i, H_i )
```

按优先级:

| 层 | 指标 | 含义 |
| --- | --- | --- |
| L1 | C_i = 1 | 硬约束:CSD 副本永远优先于 CPU-only 副本 |
| L2 | Q_i 最小 | 软约束:同能力下选择在途负载最小的服务器 |
| L3 | L_i 最小 | 软约束:选择状态探测延迟低的服务器 |
| L4 | H_i 字典序 | 可复现平局规则,保证排序确定性 |

这种设计避免为多个目标设置主观权重 α、β,使论文中“最优”的定义可以被精确检验:
只要 CSD 副本集合非空且健康,输出集合的 CSD 命中率应为 100%。

## 4. 实现

### 4.1 volume server 状态上报

`GET /compute/status`:

```json
{"csd_enabled":true,"csd_endpoint":"http://127.0.0.1:18090"}
```

### 4.2 filer 调度器

- `weed/server/filer_csd_scheduler.go`
  - `csdReplicaCapability`:CSD 能力、端点、探测延迟、在途负载;
  - `rankComputeReplicas`:按上述四层排序;
  - `rankAndReserve`:排序结果与在途负载计数原子更新,避免并发扇出争抢同一副本;
  - 状态缓存 30s。

### 4.3 自动重试

- `fetchChunkComputeResult` 按 ranked 顺序执行;
- 每个尝试前 reserve、完成后 release;
- 失败后自动尝试排名中的下一副本,直到成功或候选耗尽。

## 5. 功能正确性验证(单元测试)

- CSD vs CPU-only:连续 100 次全部首选 CSD 副本 → 100% 命中;
- 全 CSD 副本:连续 50 次排序稳定;
- 负载感知:busy(5 在途)与 idle(0 在途)的 CSD 副本,选择 idle;
- 原子预留:两个并发选择不会选到同一副本;release 后可再次选到原副本。

测试文件:`weed/server/filer_csd_scheduler_test.go`

## 6. 性能优越性论证

调度要解决的是“随机命中率约 1/n”问题。设 n 个副本中 k 个支持 CSD:

| 指标 | 随机 | CSD-aware(L1) | 负载感知(L1+L2) |
| --- | --- | --- | --- |
| CSD 命中率(理论) | k/n | 1(若 k>0) | 1(若 k>0) |
| 是否避免热点 | 否 | 不保证 | 是 |
| 可复现性 | 否 | 是 | 是 |

论文中可写:当 k=1、n=2 时,随机策略只有 50% 概率命中 CSD,
CSD-aware 策略达到 100%,并额外通过 L2 在多个 CSD 副本间实现近似最小化最大负载。

## 7. 当前边界与下一步

- 在途负载是 filer 单实例观测,多 filer 部署需改为共享/上报指标;
- `ProbeLatency` 只是状态端点延迟,尚未纳入历史执行时长 EWMA;
- 仍未在真实多副本集群上统计端到端分布;
- 下一步:真实复制集群 + 随机/CSD-aware/负载感知三组对比,
  指标包括 CSD 命中率、P50/P99、节点利用率。
