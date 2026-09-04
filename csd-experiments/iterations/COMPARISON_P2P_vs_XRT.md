# P2P 直通 vs XRT 搬移对比

## 1. 比较对象

- **V2(XRT migrate)**:`CL_MEM_USE_HOST_PTR` 创建 host buffer,
  数据先 `pread` 到主机内存,再 `enqueueMigrateMemObjects` 搬进 FPGA;
- **V3(P2P)**:`XCL_MEM_EXT_P2P_BUFFER` 创建 FPGA 内存映射,
  `pread` 直接写入映射地址,不创建等大的 host CL buffer。

## 2. 硬件/软件环境(.9)

- 设备:xilinx_u2_gen3x4_xdma_gc_2_202110_1;
- XRT 2.14.354;xclbin:file_sum64(串行累加内核);
- 数据文件:普通系统盘 `/home/dess/...` 上的 256MB 对齐文件。

## 3. 功能验证

| 场景 | V2 | V3 |
| --- | --- | --- |
| 8×uint64(offset 0) | 54 | 54 |
| 8×uint64(offset 32) | 220 | 220 |
| 512B O_DIRECT+P2P | — | 1760 |
| 256MB 全零 | 0 | 0 |

V3 日志不再出现 `unaligned host pointer ... extra memcpy`。

## 4. 256MB 端到端初步测量

| 轮次 | V2 host CL buffer | V3 P2P |
| --- | --- | --- |
| 1 | 10.30s | 12.39s |
| 2 | 10.14s | 10.86s |
| 3 | 10.14s | 10.87s |

## 5. 为什么初步测量不能证明“P2P 更慢”

### 5.1 数据源不同

V3 对齐时使用 O_DIRECT,绕过 page cache 反复读真实磁盘;
V2 使用普通 read,256MB 文件第二次起大部分命中 page cache。
因此 V2 的后两轮比较“缓存+搬移”,V3 比较“磁盘+映射”,不是同一 I/O 条件。

### 5.2 不是设备到设备 P2P

测试文件位于普通系统盘,并非 SmartSSD 自带 NVMe 的 peer-to-peer 数据通路。
当前 V3 仍由 CPU 发起 pread 写入 FPGA BAR 映射,并未验证真正的
`NVMe → FPGA` 设备 DMA。

### 5.3 内核计算主导

串行 file_sum64 内核 256MB 约 10s;数据搬移阶段的差异被计算时间掩盖,
无法通过端到端耗时单独评估搬移方案优劣。

## 6. 科学对比需要控制的条件

1. 数据位于 SmartSSD 本地 NVMe;
2. 文件/offset/size 512B 或 4K 对齐;
3. cold/warm 分开测量,或统一 drop caches;
4. 内核改为高吞吐可并行版本,让数据搬移成为可测瓶颈;
5. 分阶段计时:pread、XRT migrate、kernel、结果回传;
6. 各重复 ≥10 次,报告均值、P50/P99。

## 7. 结论写法建议

当前结论应表述为:

> P2P 已消除 host CL buffer 与 XRT 二次搬移,并消除了 unaligned extra memcpy;
> 但由于尚未在 SmartSSD 本地 NVMe、受控冷热缓存与并行内核条件下测量,
> 当前端到端数据不能用于否定 P2P 的搬移收益,该对比需要按 §6 重做。

## 8. 下一步实验矩阵

| 数据源 | cache 状态 | 内核 | 指标 |
| --- | --- | --- | --- |
| 系统盘 | cold/warm | 串行 | pread/kernel 分阶段 |
| SmartSSD NVMe | cold/warm | 串行 | 同上 |
| SmartSSD NVMe | cold/warm | 并行 16 路 | 同上 |
