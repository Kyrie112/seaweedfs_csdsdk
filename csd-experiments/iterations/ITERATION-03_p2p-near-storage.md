# V3:P2P / 盘直通 FPGA(规划模板)

> 本文件为下一迭代模板,当前尚未实现。开始 V3 后按本结构补写详细内容。

## 1. 目的

消除 V2 中“host CL buffer + XRT migrate”的主机内存中转,让数据从
盘上 `.dat` 直接 DMA 到 FPGA device memory,再进入计算内核。

## 2. 目标数据路径

```text
.dat → pread 到 FPGA P2P buffer → file_sum64 内核 → 结果
```

主机只发起读请求,不再持有整段数据。

## 3. 设计要点

- 使用 `cl_mem_ext_ptr_t` + `XCL_MEM_EXT_P2P_BUFFER`;
- `enqueueMapBuffer` 取得可直接 `pread` 的设备内存指针;
- 数据文件路径与对齐要求与 SmartSSD NVMe 对齐(如 512B/4K);
- 大区间分片、并发与错误处理。

## 4. 验证指标(填写)

| 指标 | 期望 | 实测 |
| --- | --- | --- |
| 是否还有 host memcpy | 否 | 待测 |
| pread 吞吐 | 接近 NVMe 直通 | 待测 |
| 端到端相对 V0 提升 | 待测 | 待测 |

## 5. 待补内容

- 代码位置
- 编译产物
- 真机验证记录
- 当前限制
- 论文价值
