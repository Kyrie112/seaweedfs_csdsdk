# V4:早期 CSD-aware 副本选择(基本版)

## 1. 目的

解决原 compute 请求对多副本随机选择的问题,优先把计算发送到
具备 CSD 能力的 volume server。

## 2. 时间与分支

- 时间:2026-09-04
- SeaweedFS 分支:`feat/csd-native-compute`

## 3. 目标

当副本集合中存在 CSD 副本时,选择 CSD 副本;
否则退化为普通 volume server。

## 4. 实现

- volume server 暴露 `GET /compute/status`:

```json
{"csd_enabled":true,"csd_endpoint":"http://127.0.0.1:18090"}
```

- filer 并发探测并缓存副本状态;
- 排序键(基本版):

```text
1. CSDEnabled = true 优先
2. ProbeLatency 升序
3. Host 字典序
```

## 5. 验证(单元)

- CSD vs CPU-only 连续 100 次全部命中 CSD;
- 全 CSD 副本排序稳定。

## 6. 缺陷与边界

- 只有“CSD 有/无”的硬约束,没有负载感知;
- 并发扇出可能仍把请求堆到同一 CSD 副本;
- 无副本失败重试;
- 这些能力在 V5 中补齐。
