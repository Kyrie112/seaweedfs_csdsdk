# V0:脚本 + 临时文件计算(基线)

## 1. 目的

描述最初可计算存储基线:SeaweedFS 收到 `?compute=` 请求后,由 volume 上的
Shell 脚本对单个 chunk 计算。该版本用于明确后续 CSD 改造要消除的瓶颈。

## 2. 时间与分支

- 时间:2026-08 前后
- 说明:处于“未加 CSD 原生路径”的基线版本,对应本仓库引入跨 chunk
  与多模态接口之前的 volume compute 实现。

## 3. 数据路径

```text
磁盘 volume .dat
   ↓ ReadVolumeNeedle:整段读入主机内存 n.Data
   ↓ createComputeInputFile:写入临时文件
   ↓ 执行 sum.sh / 算子脚本
   ↓ 脚本读取临时文件并按行/字节计算
   ↓ 返回结果
```

### 3.1 逐步数据流动

1. volume 根据 needle map 找到 `.dat` 内的物理位置;
2. `ReadNeedleData` 从磁盘读入 `n.Data`,实际经过
   “磁盘 → 操作系统 page cache → 主机内存 `n.Data`”;
3. 若 needle 为压缩存储,`maybeHandleComputeOperation` 先解压,产生第二次全量拷贝;
4. `createComputeInputFile` 把 `n.Data` 写入临时文件(第三次全量写入);
5. 算子脚本启动,读取临时文件并按行/字节解析;
6. 脚本 stdout 收集到 `bytes.Buffer`,再作为 HTTP body 返回。

### 3.2 缺陷分析

| 缺陷 | 说明 | 影响 |
| --- | --- | --- |
| 全量磁盘读入主机内存 | `ReadVolumeNeedle` 先读整段 chunk | 主机内存与磁盘读流量随 chunk 大小线性增长 |
| 临时文件写入/读取 | 每次计算都落盘再读 | 额外磁盘 I/O 与延迟 |
| 主机 CPU 解析 | awk/perl 等逐行处理 | CPU 成为瓶颈,带宽受限时更明显 |
| 脚本进程开销 | 每个请求 exec 一次脚本 | 启动与上下文切换开销 |
| 压缩数据需主机解压 | 先全量读压缩数据,再解压成明文 | 多一次全量内存拷贝与 CPU 解压 |
| 早期仅单 chunk | filer 只支持单 chunk 覆盖整个文件 | 大文件无法直接计算 |

这些缺陷构成论文中“计算下沉到 CSD”的动机基线。

## 4. 代码位置

- 读请求入口:`weed/server/volume_server_handlers_read.go`
  - 先执行 `ReadVolumeNeedle`,将 needle 数据整段读入 `n.Data`;
- 计算脚本:`weed/server/volume_compute.go`
  - `createComputeInputFile` 创建临时文件;
  - `runComputeScript` 通过 `exec.CommandContext` 执行脚本;
  - 脚本通过 `SEAWEED_COMPUTE_INPUT_FILE`、stdin 等环境变量拿到数据。

## 5. 关键开销

1. 数据从 SSD 全量读入主机内存(`n.Data`);
2. 再从内存写入临时文件(第二次全量拷贝);
3. 脚本启动、解释执行并按行解析数据;
4. 大文件原本只能单 chunk 处理,超过 `-maxMB` 后无法直接计算。

（更细的逐步数据流与缺陷见上文 3.1/3.2。）

## 6. 验证结果(实验报告)

已有实验数据见:

- `csd-experiments/REPORT.md`
- `csd-experiments/REPORT_SERVER.md`
- `csd-experiments/REPORT_SERVER_TABLE.md`

结论:当主机 CPU、带宽受限时,下载 + 脚本计算链路耗时显著上升,
这正是论文中论证“计算应下沉到存储侧”的实验基础。

## 7. 论文可引用结论

- 当前系统对单个 chunk 采用“主机内存 + 临时文件 + 脚本”的通用方案;
- 灵活但代价高:数据被多次搬运,计算发生在通用 CPU;
- 为近存/CSD 改造提供了明确的优化指标:消除临时文件、消除主机内存整段拷贝、
  让计算在数据所在设备附近执行。

## 8. 下一迭代

在 V1 中先解决“跨 chunk + 多模态上层调用”,在 V2 中替换底层执行引擎。
