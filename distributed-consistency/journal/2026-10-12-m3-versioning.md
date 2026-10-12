# 开发日志 2026-10-12:M3 版本化快照与延迟回收

> 里程碑:[M3 版本化快照](../M3-versioning.md) · 状态:**✅ 完成**(T3.1–T3.8)
> 前置:[M1 可检测](2026-10-12-m1-detection.md)、[M2 可规避](2026-10-12-m2-mitigation.md)

## 0. 结论摘要

| 项目 | 结果 |
| --- | --- |
| 目标 | 计算进行中对同一卷做整理,**在途请求仍读到它开始时的数据**;旧版本在无人使用后被回收 |
| 实现方式 | **硬链接保留版本**(见第 2 节的方案调整) |
| 单元/集成测试 | 5 个单元测试 + 1 个存储层集成测试,全部通过 |
| 集群验证 | 3 个 volume server(副本因子 001):版本文件按预期生成,GC 端点正确拒绝回收当前代次 |
| 端到端矩阵 | F1/F4/F7 全部 PASS,**静默错误 0** |
| 性能 | 计算请求中位 48.27 ms(1024 张)/ 25.98 ms(2048 张),与 M2 同量级 |

## 1. 本次实施范围

按 [M3-versioning.md](../M3-versioning.md) 执行 T3.1–T3.8。

## 2. 方案调整:用硬链接而不是"改名保留"

原计划是"整理时把旧 `.dat` 改名为带代次的文件",实现时发现一个更简单、风险更低的做法:

**为当前数据文件建立一个硬链接 `<base>.dat.g<代次>`,并让计算描述符指向这个链接。**

- 硬链接与 `.dat` 指向**同一个 inode**,不复制数据、不占额外空间;
- 整理提交时那句 `os.RemoveAll(v.FileName(".dat"))` 只删除**目录项**;
  由于版本链接仍指向该 inode,**旧数据依然可读**,而新数据由新的 `.dat` 提供;
- 因此散落整理(compaction)的提交路径几乎不需要改动——只在提交成功后
  自增代次并触发一次回收;
- 描述符本来就携带完整物理区间,把路径换成版本链接即可让老请求"按老版本走",
  **协议无需任何改动**。

这一步把 M3 从"改动数据提交路径的高风险改造"变成"新增版本链接 + 代次 + 回收策略",
风险显著下降。T3.3 因此从"重写提交逻辑"简化为"提交后自增代次 + 不删除版本链接"。

## 3. 开发过程

| 任务 | 实现 | 位置 |
| --- | --- | --- |
| T3.1 代次 | `versionState{generation, lastUsed}`,整理提交后自增;启动时从磁盘上已有的版本文件恢复,避免重启后复用代次 | `weed/storage/volume_versioning.go`、`volume_vacuum.go` |
| T3.2 版本布局 | `<base>.dat.g<N>`;后缀不是 `.idx`/`.vif`,**不会被卷扫描当成新卷**(有专项测试) | 同上 |
| T3.3 整理保留 | 提交完成后 `bumpDataGeneration()` + 回收;`RemoveAll(.dat)` 因硬链接存在而不会真正丢数据 | `weed/storage/volume_vacuum.go` |
| T3.4 描述符带版本 | `NeedleComputeRegion` 返回版本链接路径,并记录 `Generation`;取链接在**读锁**内完成,与整理的写锁互斥,保证链接指向整理前的 inode | `weed/storage/volume_read.go` |
| T3.5 使用登记 | `noteVersionUse(generation)`:每次生成描述符时登记"该版本正在被使用";启动时给所有现存版本一个完整的保留窗口(不信任可能很旧的文件 mtime) | `volume_versioning.go` |
| T3.6 回收策略 | 回收条件:**非当前代次** ∧ 超过保留期(默认 10 分钟) ∧ 不超过"最多保留 N 个历史版本"(默认 4) | 同上 |
| T3.7 与 M1/M2 配合 | 强制回收后,尚未完成的请求会命中 M1 的 `stale_range`,再由 M2 重试或显式失败 | 已有机制 |
| T3.8 观测与回归 | 新增卷级端点 `GET/POST /compute/versions`(查看保留版本 / 按指定保留期回收) | `weed/server/csd_compute.go`、`volume_server.go` |

**端口说明**:`GET /compute/versions?volume=N` 返回当前保留的版本及其最近使用时间;
`POST` 同路径可用 `retention=1s` 强制一次回收,便于运维与验证。

## 4. 测试

### 4.1 单元测试(`weed/storage/volume_versioning_test.go`)

| 测试 | 验证内容 | 结果 |
| --- | --- | --- |
| `TestVersionLinkSurvivesDataFileSwap` | 替换在线 `.dat` 后,版本链接仍读到**原始数据**,且在线路径确实是新数据 | PASS |
| `TestVersionsAreNotReclaimedWhileInUse` | 保留期内不回收;当前代次永不回收 | PASS |
| `TestExpiredVersionsAreReclaimed` | 超过保留期后历史版本被真正删除,当前代次保留 | PASS |
| `TestVersionFilesLookNothingLikeVolumes` | 版本文件不会被卷扫描认领 | PASS |
| `TestGenerationIsRecoveredAcrossRestart` | 重启后代次恢复,不复用旧编号 | PASS |

### 4.2 存储层集成测试(`volume_versioning_integration_test.go`)

`TestComputeRegionReadsRetainedVersion` 走的是**真实路径**:

```text
真实写入 needle(带 CRC)
  → NeedleComputeRegion 取得区间(返回版本链接路径 + 代次)
  → 读版本链接,内容与写入一致
  → 替换在线 .dat(模拟整理提交)
  → 再次读版本链接,内容仍是原始数据   ← M3 的核心保证
  → 代次自增后立即回收:不删(仍在保留期内)
  → 超过保留期后回收:删掉,当前代次保留
```

**PASS**。这是本阶段最关键的一条证据。

## 5. 集群验证(本地三节点形态)

> **说明**:计划中的 tan09/tan10/tan11 在本容器内**不可达**(名称无法解析,
> 且容器没有到 192.168.0.x 的路由)。因此本轮用本地三节点形态替代:
> master × 1、volume server × 3(各配一个 CSD agent)、filer × 1,副本因子 001。
> 另提供了可直接在 tan 集群运行的部署脚本(见第 7 节)。

脚本:`csd-experiments/scripts/run_local_multinode.sh`,原始数据:
`csd-experiments/results/m3_multinode.csv`。

| 检查项 | 结果 |
| --- | --- |
| 副本放置 | 卷 1 位于 vol0/vol1,卷 2、3 位于 vol0/vol2,卷 5、6 位于 vol1/vol2 → **每个卷确有 2 个副本** |
| 计算正确性 | 与暴力检索逐位一致 |
| 版本文件生成 | 服务的卷上出现 `vol0/1.dat.g0`、`vol1/1.dat.g0`(两个副本各自保留了自己的版本) |
| 版本观测端点 | `GET /compute/versions?volume=1` 返回版本路径、代次、是否当前代次、最近使用时间 |
| GC 语义 | `POST ...&retention=1s` 后**当前代次未被删除**(符合"永不回收当前代次") |

**未在本轮集群中执行的部分(如实记录)**:触发一次**真实整理**以使代次前进,
从而在集群上观察"历史版本被回收"。原因是本地难以稳定制造可回收垃圾并触发 vacuum。
该路径已由 4.2 的存储层集成测试完整覆盖(含"超过保留期后回收");集群侧补充验证
需要 tan 集群或可控的 vacuum 触发手段(登记为遗留 R10)。

## 6. 性能记录

| 场景 | 数值 | 说明 |
| --- | --- | --- |
| 计算请求(1024 张 / 1 分块,3 次中位) | 48.27 ms | 三节点形态,与 M2 同量级 |
| 计算请求(2048 张 / 2 分块,3 次中位) | 25.98 ms | 同上 |
| 回传数据量 | 42–47 字节 | 与改造前一致 |
| 版本链接开销 | 每个代次一次 `os.Link`(元数据操作,不复制数据) | 单卷每代次可忽略 |
| 额外磁盘占用 | **0**(硬链接共享 inode) | 直到旧版本被回收前,旧数据本身仍占空间,但这是"保留"的语义而非额外副本 |

**结论**:M3 在正常路径上只增加"每代次一次硬链接创建"的元数据开销;
空间代价体现为"被保留的旧版本所占的空间(至多 N 个历史版本)",
而不是数据复制。真正的空间放大取决于整理频率与保留期,需要更长时间的观测(遗留 R11)。

## 7. tan 集群部署脚本

由于本容器无法访问 tan09/tan10/tan11,已提供
`csd-experiments/scripts/deploy_csd_cluster.sh`,在有网络的机器上执行:

```bash
HOSTS="tan09 tan10 tan11" ./deploy_csd_cluster.sh start     # 部署
HOSTS="tan09 tan10 tan11" ./deploy_csd_cluster.sh status    # 检查
HOSTS="tan09 tan10 tan11" ./deploy_csd_cluster.sh verify    # 一致性 + 版本保留验证
HOSTS="tan09 tan10 tan11" ./deploy_csd_cluster.sh stop
```

拓扑:`tan09` 跑 master + filer,三台都跑 volume server + 本地 CSD agent。

## 8. 遗留问题

| 编号 | 事项 | 归属 |
| --- | --- | --- |
| R10 | 集群上"真实整理 → 代次前进 → 历史版本回收"需在 tan 集群验证 | M4 / 后续 |
| R11 | 空间放大随整理频率与保留期的关系需要长时间观测定标 | 后续 |
| R12 | 精确引用计数(按请求而非按"最近使用时间")需要跨节点引用表;当前用 TTL 兜底 | 后续 |
| R13 | 卷重启会丢失内存中的使用记录;当前用"启动时给所有现存版本一个完整保留窗口"兜底 | 已有缓解 |
| R14 | 长时间运行(超过保留期)的请求仍可能被回收打断,由 M1/M2 兜底 | 与 R12 同源 |
