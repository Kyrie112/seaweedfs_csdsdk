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

### 2.1 逐步数据流动(目标)

1. SeaweedFS 只下发 `.dat` 路径、offset、size;
2. agent 用 XRT P2P buffer(`XCL_MEM_EXT_P2P_BUFFER`);
3. `enqueueMapBuffer` 返回 FPGA device memory 对应的主机可访问指针;
4. `pread` 直接写入该 P2P 指针,数据由 DMA 进入 FPGA DDR;
5. 内核直接读取 P2P 区域计算;
6. 仅结果返回主机。

### 2.2 待消除的缺陷

| 缺陷 | V2 现状 | V3 目标 |
| --- | --- | --- |
| host 侧等大缓冲区 | 存在 | 消除 |
| host→FPGA 二次迁移 | 存在 | 由 pread DMA 直通替代 |
| unaligned extra memcpy | 真机日志已出现 | 使用 XRT P2P 缓冲区避免 |
| O_DIRECT/页对齐 | 未处理 | 与 NVMe 对齐配合 |

### 2.3 仍保留的限制

- P2P 仍是“盘数据通过 PCIe DMA 进入 FPGA”,不是设备内部直读;
- 若 volume `.dat` 不在 SmartSSD 本地,仍需网络传输;
- 完全不出盘需要设备固件/厂商 NVMe 命令支持,属于后续版本。

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
