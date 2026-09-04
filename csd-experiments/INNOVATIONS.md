# 相对上游 SeaweedFS 的创新点说明

本文说明本工作基于原始 SeaweedFS 增加了哪些具有研究价值的改动,
用于毕业论文“工作与创新点”章节。每个创新点均给出动机、做法、
验证状态与对应代码位置。

## 1. 文件 / 对象 / 块多模态计算调用

**上游能力**:SeaweedFS 的计算入口本质上只是单个 volume 上的一段 HTTP/脚本逻辑,
不支持跨 chunk,也没有面向用户的多模态接口。

**创新做法**:

- 统一计算入口 `GET /api/compute/{file|object|block}/<resource>?compute=<op>`;
- 文件、对象、块三种寻址共享同一套 filer 编排;
- S3 兼容接口通过 `?x-compute=` / `X-SeaweedFS-Compute` 触发;
- 普通文件(S3/FUSE)语义保持不变,计算请求作为独立上层控制面存在。

**验证**:22 chunk 文件在五种入口均返回一致结果。

**代码**:

- `weed/server/filer_compute_api.go`
- `weed/s3api/s3api_compute.go`

## 2. 跨 chunk 计算编排(Map / Reduce)

**上游能力**:原 volume compute 只接受“单个 chunk 覆盖整个文件”的数据。

**创新做法**:

- filer 解析并展平 chunk manifest;
- 校验 chunk 从 offset 0 连续覆盖文件,避免空洞/重叠;
- 并发(上限 8)把同一算子扇出到各 chunk 所在 volume;
- filer 用 `big.Int` 对每 chunk 的部分结果做数值聚合;
- 对不可合并的非数值算子显式拒绝,不产生错误聚合。

**验证**:184.5MB / 22 chunk 的 `rawsum64` 与 CPU 参考一致。

**代码**:`weed/server/filer_server_handlers_proxy.go`

## 3. “数据区间描述符”式近存计算协议

**上游能力**:volume 处理 `?compute=` 时先把整个 needle 读入主机内存,
再写临时文件执行脚本。

**创新做法**:

- volume 只读 needle 元数据得到 `.dat` 的物理 offset/size;
- 不再整段读入 `n.Data`,不再创建临时文件;
- 将 `{data_file, offset, size, operation}` 交给 CSD agent;
- 压缩/manifest/legacy 场景自动回退 CPU 脚本,保证可用性。

**验证**:本地集群 + 模拟 agent 验证区间描述正确;压缩文件正确回退。

**代码**:

- `weed/storage/volume_read.go`(`NeedleComputeRegion`)
- `weed/server/csd_compute.go`
- `weed/server/volume_server_handlers_read.go`

## 4. SmartSSD 常驻计算 agent 与 P2P 数据通路

**上游能力**:只有主机侧 OpenCL 样例,每个请求重新加载 xclbin,
数据经 host 内存迁入 FPGA。

**创新做法**:

- CSD agent 常驻运行,xclbin 只加载一次;
- `POST /v1/compute` 接收区间描述;
- V3 使用 `XCL_MEM_EXT_P2P_BUFFER` + `enqueueMapBuffer`,
  O_DIRECT 对齐时直接把盘上区间读入 FPGA device memory;
- 输出缓冲对齐,消除 XRT `unaligned host pointer ... extra memcpy`。

**验证(192.168.0.9 真机)**:

- 512B O_DIRECT + P2P:期望 1760,返回 1760;
- 256MB 全零:返回 0,无 unaligned 警告。

**代码**:

- `smartssd_compute_module/src/csd_agent.cpp`
- `smartssd_compute_module/src/file_sum64_kernel.cpp`

## 5. CSD-aware 副本选择策略

**上游能力**:compute 请求对多副本采用 `rand.IntN(len(urls))` 随机选择,
不感知 CSD 能力、负载与健康。

**创新做法**:

- volume server 新增 `GET /compute/status` 上报 CSD 能力;
- filer 并发探测并缓存副本能力;
- 分层(lexicographic)排序:

```text
L1 CSD 能力(硬约束)
L2 filer 侧在途负载(软约束)
L3 状态探测延迟
L4 host 字典序(确定性)
```

- `rankAndReserve` 原子预留负载,避免并发扇出集中到同一副本;
- 按 ranked 顺序执行,失败自动切换下一最优副本。

**验证(单元)**:

- CSD vs CPU-only:连续 100 次 100% 命中 CSD;
- 负载感知:在途 5 的 busy 副本会被跳过;
- 并发两次 reserve 不会选择同一副本。

**代码**:

- `weed/server/filer_csd_scheduler.go`
- `weed/server/filer_csd_scheduler_test.go`

## 6. 面向论文的工程化与可复现资产

- SOP:每类存储接口的标准操作流程;
- 迭代文档 V0–V4:从脚本基线到 P2P/调度演进的完整记录;
- 控制变量说明:P2P vs XRT 的对比与尚未满足的实验条件被显式记录,
  避免把初步测量误读为最终结论。

## 结论写法建议

> 本工作在 SeaweedFS 上引入“数据面语义不变、计算面独立”的多模态计算模型;
> 将计算从主机脚本演进为基于区间描述符的 CSD 近存计算,并在 SmartSSD 上
> 实现 P2P 数据通路;同时设计了可验证的 CSD-aware 副本调度,
> 使计算请求能稳定命中最优可计算副本。
