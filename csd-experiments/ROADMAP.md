# SeaweedFS + SmartSSD 可计算存储后续工作计划

本文是现有 V0–V5 融合工作之上的后续路线图,用于指导毕业论文的
“系统实现 + 模型 + 实验”三部分内容扩展。计划分为三个阶段,
每个工作项给出动机、现状、做法与可验证的验收标准。

## 1. 已完成的能力基线

| 能力 | 状态 | 主要代码 |
| --- | --- | --- |
| 文件/对象/块多模态计算入口 | 已完成 | `weed/server/filer_compute_api.go`、`weed/s3api/s3api_compute.go` |
| 跨 chunk 编排与聚合(Map/Reduce) | 已完成 | `weed/server/filer_server_handlers_proxy.go` |
| 区间描述符式近存计算协议 | 已完成 | `weed/storage/volume_read.go`(`NeedleComputeRegion`)、`weed/server/csd_compute.go` |
| SmartSSD 常驻 agent + P2P 通路 | 已完成(设计) | `smartssd_compute_module/src/csd_agent.cpp` |
| CSD-aware 副本调度(能力/负载/延迟) | 已完成 | `weed/server/filer_csd_scheduler.go` |

融合已经跑通,但距离一篇扎实的毕业设计还缺三样东西:
**可量化的理论模型、更深的机制、更硬的实验证据**。

## 2. 总体目标与三条主线

- **主线 A(搬移最小化)**:把“零拷贝/近存计算”从设计文档落到实测数据上,
  让“减少数据搬移与传输带宽”成为可度量的结论。
- **主线 B(调度模型化)**:把启发式字典序排序升级为可推导、可优化、
  可解释的调度模型。
- **主线 C(实验体系)**:建立指标、消融、规模、尾延迟与能耗的完整实验矩阵。

## 3. 阶段一(2–3 周):把核心卖点做实

本阶段的定位是**修复三个已被定位的真实缺陷**,并把观测手段补齐,
为后续所有实验提供可信的数据来源。

### P1-1 修复 P2P 对齐通路(O_DIRECT 实际未生效)

**现状**:`csd_agent.cpp:198` 用 `offset % 512 == 0 && size % 512 == 0`
判断能否使用 `O_DIRECT`,但 volume 侧给出的数据区起点是
`actualOffset + NeedleHeaderSize + DataSizeSize`(`volume_read.go:78`,即固定 `+20`),
needle 在 `.dat` 中又无填充对齐。两者叠加导致该判定在实际业务请求下
几乎恒为假,真实请求走的是普通 `pread`(page cache + 主机内存),
“P2P + O_DIRECT”只在人工构造的对齐请求下成立。

**做法**:

1. 扩展 SeaweedFS → CSD agent 的区间描述符,增加对齐窗口字段:
   `read_offset`(payload 起点向下取整到 512)、
   `read_size`(覆盖 payload 的对齐长度)、
   `payload_shift`(`offset - read_offset`);
2. agent 以 `read_offset/read_size` 打开 `O_DIRECT` 读入 P2P buffer;
3. FPGA 内核升级为 v2:新增 `payload_shift` 与 `payload_size` 参数,
   使打包权重从 payload 首字节起算,保证与“从 payload 首地址重新打包”的
   语义逐字节一致(否则整体左移会改变 uint64 打包权重);
4. 兼容性:agent 启动时用 `CL_KERNEL_NUM_ARGS` 探测内核版本,
   老 xclbin 自动回退 v1 语义,**绝不静默改变计算结果**。

**验收标准**:

- 主机侧参考模型证明 v1/v2 语义在 payload 已对齐时结果完全一致;
- 非对齐请求实测进入 `O_DIRECT` 分支(agent 输出 `direct_hits/direct_misses` 计数);
- 真机 8MiB needle 求和结果与 CPU 参考一致。

**产出素材**:修复前后端到端时延与主机内存流量对比曲线。

### P1-2 读取完整性校验

**现状**:`csd_agent.cpp:206` 的 `pread` 只判断 `got < 0`,
不校验 `got` 是否等于请求长度,短读会静默返回错误结果。
`O_DIRECT` 对长度与对齐有额外约束,更需要显式校验。

**做法**:改为循环读直到读满,读不满即返回明确错误;
新增 `direct_hits/direct_misses/short_reads` 计数便于验证与观测。

**验收标准**:主机侧单元测试覆盖“短读必须报错”“读满才返回结果”;
对同一 payload,分片 `pread` 与一次性读取结果一致。

### P1-3 调度器加固(锁粒度 / 失败重试 / 字节加权)

**现状**:`filer_csd_scheduler.go` 中存在三个问题:

1. `rankAndReserve` 在持锁状态下调用 `rankComputeReplicas`,
   而后者包含网络状态探测,导致并发扇出被串行化;
2. `fetchChunkComputeResult` 的重试循环没有排除集,
   失败后可能重新选中刚失败的副本,使 failover 形同虚设;
3. 在途负载按**请求数**计,大 chunk 与小 chunk 权重相同,
   负载估计与真实带宽占用不符。

**做法**:

1. 探测结果继续用 30s TTL 缓存,排序只读内存快照,
   临界区缩小到“选点 + 预留”一步;
2. 每次重试传入已失败副本的排除集,保证重试命中不同副本;
   同一次请求的副本排名只计算一次并复用;
3. 在途负载改为按字节加权(`weight = read_size`),
   排名依次为:CSD 能力(硬约束)→ 加权在途字节(软约束)→ 探测延迟 → host 字典序。

**验收标准**:Go 单元测试覆盖排除集、并发预留互斥、字节加权排序;
故障注入实验证明重试必然切换副本。

### P1-4 分阶段耗时观测

**现状**:代码中没有任何分段计时,无法回答“时间花在调度、排队、
设备读、内核还是回传”。

**做法**:agent 返回 `read_ms/kernel_ms`;volume server 通过响应头透传;
filer 记录每 chunk 与整请求的 dispatch/总耗时,并在日志中输出。

**验收标准**:一次多 chunk 请求可导出逐 chunk 的阶段耗时表,
用于绘制瀑布图。

### 阶段一验收清单

实施与验证结果见 [reports/REPORT_PHASE1_HARDENING.md](reports/REPORT_PHASE1_HARDENING.md)。

- [x] P1-1 对齐读路径与内核 v2(含兼容回退)
- [x] P1-2 短读/部分读校验
- [x] P1-3 锁粒度、failover 排除集、字节加权负载
- [x] P1-4 分阶段耗时导出
- [x] Go 单元测试与 `go build ./weed/server/ ./weed/storage/` 通过
- [x] 主机侧参考模型测试通过(等价性证明,354 项检查)
- [ ] 真机 xclbin 重合成与端到端实测(需 .9 机器,阶段一收尾)

## 4. 阶段二(3–4 周):从启发式调度到调度模型

这是让毕设从“工程报告”升级为“研究工作”的关键阶段。

1. **下沉判决成本模型**:以数据量、链路带宽、算子计算强度为变量,
   推导“近存计算优于主机计算”的 break-even 不等式,给出适用边界。
2. **整文件全局规划**:当前每个 chunk 独立贪心选副本,
   调度器看不到 chunk 大小与拓扑。改为整请求一次性规划,
   目标函数为最小化 makespan 或最大链路负载,约束为各 volume server
   的带宽与设备并发,给出 LP 松弛与贪心近似两种解法并对比。
3. **自适应并发**:替换硬编码的 `computeChunkFanoutConcurrency = 8`
   与 `16/host` 信号量,改为基于 RTT 与队列时延的反馈控制(AIMD),
   并验证其在带宽受限与设备争抢场景下的稳定性。
4. **拓扑感知放置**:引入机架/交换机亲和,降低跨机架 chunk 比例。

**验收标准**:给出调度模型的公式推导、近似算法的复杂度分析,
以及与逐 chunk 贪心的对比实验(时延、尾延迟、带宽利用率)。

## 5. 阶段三(2–3 周):算子扩展与实验体系

1. **可结合算子代数**:把 sum/min/max/count/histogram/Welford 在线方差、
   点积、汉明距离统一到 partial-reduce 框架;
   对分位数/中位数等不可结合算子设计“本地草图 + 全局合并”两阶段方案。
2. **谓词与范围下推**:`ResolveChunkManifest` 已支持 `startOffset/stopOffset`,
   但被写死为 `0, fileSize`;将其暴露到三种上层接口,
   做选择性扫描实验,直接量化带宽节省。
3. **压缩 needle 的 CSD 路径**:当前遇到压缩 needle 直接回退 CPU 解压。
4. **纠删码分片上的计算**:探索在 EC 分片上直接计算再合并的可行性。
5. **实验体系**:消融矩阵(关 P2P / 关 CSD / 关调度器 / 关跨 chunk)、
   规模扩展(多 volume server、多盘、更大文件)、真实数据集、
   三组基线(宿主 CPU、本地 pandas/Spark、XRT 全搬移),
   并补充 **尾延迟 P99/P999、公平性 Jain 指数与能耗** 指标。

## 6. 论文章节映射

| 论文章节 | 对应工作 | 可产出的图表 |
| --- | --- | --- |
| 第三章 系统设计 | 已完成基线 + 阶段一协议扩展 | 架构图、区间描述符字段表 |
| 第四章 关键机制 | P1-1/P1-2/P1-3、阶段二调度模型 | 对齐读数据通路图、调度目标函数与算法伪码 |
| 第五章 实验 | P1-4、阶段二对比、阶段三实验体系 | 阶段耗时瀑布图、消融对比、尾延迟与能耗曲线 |
| 第六章 结论与展望 | EC 分片计算、跨节点规划 | 适用边界(break-even)分析 |

## 7. 风险与回退

- **内核 v2 需要重新合成 xclbin(约 2 小时)**:必须保留 v1 兼容路径,
  未更新 xclbin 的节点自动回退,不影响可用性。
- **对齐读改变结果语义的风险**:通过主机侧参考模型测试先行验证,
  再上真机。
- **调度模型过度设计**:阶段二先给出可推导的目标函数与一个近似算法,
  保证实验闭环优先于模型复杂度。
