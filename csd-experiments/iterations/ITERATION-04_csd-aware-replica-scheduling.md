# V4:CSD-aware 副本选择策略

## 1. 目的

解决 compute 请求在多个数据副本之间“随机选择”的问题,
让每个 chunk 的计算尽量命中 CSD 能力最优的 volume server,
避免随机打到 CPU-only 或能力弱的副本。

## 2. 时间与分支

- 时间:2026-09-04
- SeaweedFS 分支:`feat/csd-native-compute`
  - 提交:`0a4b05919 feat: CSD-aware compute replica scheduling with status probe and ranking`

## 3. 背景缺陷

原逻辑在 `fetchChunkComputeResult` 中:

```go
target = urlStrings[rand.IntN(len(urlStrings))]
```

即每个 chunk 的副本选择完全随机:

- 不知道哪个 volume server 有 CSD agent;
- 无法稳定命中 SmartSSD;
- 副本故障/能力不一致时无感知;
- 论文难以宣称“计算下沉到最优可计算副本”。

## 4. 实现

### 4.1 volume server 状态端点

- `weed/server/csd_compute.go`:新增 `csdStatusHandler`;
- volume server 暴露 `GET /compute/status`:

```json
{"csd_enabled":true,"csd_endpoint":"http://127.0.0.1:18090"}
```

### 4.2 filer 副本打分

- `weed/server/filer_csd_scheduler.go`:
  1. filer 对每个 chunk lookup 得到所有副本 URL;
  2. 并发探测 `/compute/status`,结果缓存 30s;
  3. 打分排序:
     - CSD-enabled 副本优先;
     - 同能力按探测延迟升序;
     - 相同则 host 字典序稳定;
  4. `fetchChunkComputeResult` 改为取排序后的第一名。

### 4.3 失败/退化

- 状态端点不存在或探测失败时,按能力 false 处理;
- 所有副本都不是 CSD 时,退化为延迟/字典序排序,仍走原有 volume compute。

## 5. 验证(单元)

构造两个副本:

- 副本 A:`/compute/status` 返回 `csd_enabled=true`;
- 副本 B:`csd_enabled=false`。

`TestRankComputeReplicasPrefersCSD`:连续 100 次排名,全部首选 A,
命中率 100%;随机策略的理论命中率约 50%,说明调度策略显著优于随机。

`TestRankComputeReplicasStableWhenAllCSD`:三个全 CSD 副本,连续 50 次
排序稳定,便于缓存与负载均衡。

## 6. 当前限制

- 单元测试验证的是“策略排序正确”,尚未在真实多副本集群上端到端统计
  各 volume server 接收的 compute 请求分布;
- 尚未加入动态健康/负载/时延反馈,只做了 CSD 优先与探测延迟;
- `/compute/status` 需要新分支 volume server 同时部署;
- CSD agent 失败时仍未自动切换副本(后续可用 ranked 列表实现重试)。

## 7. 论文价值

- 将“随机副本选择”升级为“CSD 能力感知调度”;
- 可证明计算请求稳定命中可计算存储节点,避免 CPU-only 副本;
- 为后续加入副本健康、负载、就近与 CSD agent 状态提供接口基础。

## 8. 下一迭代

- 真实复制集群(volume 多副本)上统计随机 vs CSD-aware 的请求分布;
- 失败自动重试下一个 ranked 副本;
- 将 CSD agent 健康/在途任务纳入打分。
