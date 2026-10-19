# V5:增强型 CSD-aware 副本调度(负载感知 + 原子预留 + 失败重试)

## 1. 目的

在 V4 的 CSD 能力优先基础上,新增两层能力:

1. 同一能力级别内选择在途负载最小的副本;
2. 多个并发 chunk 扇出时,通过原子预留避免全部命中同一副本;
3. 首选副本失败时,自动切换到排名中的下一最优副本。

## 2. 时间与分支

- 时间:2026-09-04
- SeaweedFS 分支:`feat/csd-native-compute`
  - 本地代码提交:`8d9eaedc1`
  - 远程对应提交:`0bd24ea9b`

## 3. 相比 V4 新增功能

| 功能 | V4 | V5 |
| --- | --- | --- |
| CSD 能力硬约束 | ✓ | ✓ |
| 状态探测延迟 | ✓ | ✓ |
| filer 侧在途负载 | ✗ | ✓ |
| 并发扇出原子预留 | ✗ | ✓ |
| 副本失败自动重试 | ✗ | ✓ |
| 排序可复现 | ✓ | ✓ |

## 4. 调度目标函数

```text
minimize ( -C_i, Q_i, L_i, H_i )
```

- `C_i`:是否具备 CSD 能力(硬约束);
- `Q_i`:filer 侧在途负载(软约束);
- `L_i`:状态探测延迟;
- `H_i`:host 字典序(确定性)。

## 5. 实现

### 5.1 在途负载

Filer 在 `csdInflight map[string]int64` 中记录每个副本上
“已派发但未返回”的 compute 请求数。

### 5.2 rankAndReserve

`rankAndReserve` 在选择副本的同时原子增加该副本的 in-flight 计数,
返回 release 函数;请求结束(成功或失败)后释放。

```go
func (fs *FilerServer) rankAndReserve(...) (string, func(), error)
```

### 5.3 失败重试

`fetchChunkComputeResult` 按 ranked 顺序逐个尝试:

```text
选择第 1 优副本 → reserve → 执行
  ├─ 成功 → release → 返回结果
  └─ 失败 → release → 选择第 2 优副本 ...
```

## 6. 验证结果

### 6.1 功能正确性(单元)

- CSD vs CPU-only:连续 100 次 100% 命中 CSD;
- 负载感知:busy(5 在途)与 idle(0 在途)选择 idle;
- 原子预留:两个并发 reserve 选择不同副本;release 后可再次选择原副本;
- 排序稳定:全 CSD 副本连续 50 次结果一致。

### 6.2 性能优越性论证

设 n 个副本中 k 个支持 CSD:

| 指标 | 随机 | V4 基本版 | V5 |
| --- | --- | --- | --- |
| CSD 命中率 | k/n | 100%(若 k>0) | 100%(若 k>0) |
| 避免同一节点热点 | 否 | 不保证 | 是 |
| 首选失败后可用性 | 低 | 低 | 自动切换副本 |

### 6.3 说明

性能“优越性”当前通过调度性质与单元测试验证:
CSD 命中率、负载分散、可复现、失败重试。
真实多副本集群的端到端 P50/P99 对比列为下一阶段实验。

## 7. 当前限制

- `Q_i` 是 filer 单实例视角,多 filer 部署需共享负载视图;
- 状态端点探测延迟不等于任务真实执行时长,后续可引入历史执行 EWMA;
- volume/CSD agent 的真实队列长度尚未上报;
- 仍需真实复制集群端到端统计验证。

## 8. 代码

- `weed/server/filer_csd_scheduler.go`
- `weed/server/filer_server_handlers_proxy.go`
- `weed/server/filer_csd_scheduler_test.go`
