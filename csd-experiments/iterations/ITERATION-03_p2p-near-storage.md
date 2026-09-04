# V3:P2P / 盘直通 FPGA

## 1. 目的

在 V2 agent 基础上消除“host CL buffer + XRT migrate”的主机内存中转,
让 `.dat` 区间通过 XRT P2P 映射直接写入 SmartSSD FPGA 的 device memory。

## 2. 时间与分支

- 时间:2026-09-04
- CSD 模块分支:`feat/csd-native-compute`
  - 提交:`938de41 feat: P2P pread straight into FPGA memory via XCL_MEM_EXT_P2P_BUFFER`
- SeaweedFS 侧协议不变,仍为 `POST /v1/compute {operation,data_file,offset,size}`。

## 3. 数据路径

```text
磁盘 .dat 区间
   ↓ O_DIRECT + pread 到 P2P 映射指针
FPGA device memory(XCL_MEM_EXT_P2P_BUFFER)
   ↓ file_sum64 内核直接读
8B 结果返回
```

V2 与 V3 对比:

```text
V2: .dat → host CL buffer → XRT migrate → FPGA DDR → kernel
V3: .dat → O_DIRECT/P2P → FPGA DDR → kernel
```

## 4. 实现要点

- 使用 `cl_mem_ext_ptr_t + XCL_MEM_EXT_P2P_BUFFER` 创建输入 buffer;
- `queue_->enqueueMapBuffer` 返回 FPGA device memory 在主机地址空间中的映射;
- `pread` 直接把盘上区间写入映射指针;
- offset 与 size 均满足 512B 对齐时启用 `O_DIRECT`,实现盘 → FPGA 的直读;
- 输出 buffer 改为对齐分配,消除 V2 中的 `unaligned host pointer` 警告。

## 5. 真机验证(.9)

### 5.1 小请求(非 512 对齐)

数据 `[3,10,17,24,31,38,45,52]`,期望 220:

| offset | size | 结果 |
| --- | --- | --- |
| 0 | 64 | 54(对应文件前 64B 内容,正确) |
| 32 | 64 | 220 ✓ |

### 5.2 O_DIRECT + P2P(512B 对齐)

数据为 `[3,10,17,24,31,38,45,52] × 8`,512B,期望 1760:

```text
offset=0, size=512 → {"result":"1760"} ✓
```

两次验证均未再出现:

```text
[XRT] WARNING: unaligned host pointer detected, this leads to extra memcpy
```

### 5.3 256MB 效率对比(真机)

数据:256MB 全零、512B 对齐;每个模式 3 轮,agent 常驻;

| 轮次 | V2 host CL buffer | V3 P2P(O_DIRECT) |
| --- | --- | --- |
| 1 | 10.30s | 12.39s(冷读) |
| 2 | 10.14s | 10.86s |
| 3 | 10.14s | 10.87s |

功能正确:每轮均返回 0。

分析:当前 file_sum64 为串行累加内核,计算本身主导端到端时间,
P2P 在“数据搬运阶段”的优势被计算/磁盘读掩盖;从日志看 V3 不再出现
`unaligned host pointer ... extra memcpy`。真正体现 P2P 收益需要:
更高吞吐并行内核、真实 SmartSSD NVMe 路径、按 pread/migrate/kernel 分阶段计时。

## 6. 相比 V2 消除的缺陷

| 缺陷 | V2 | V3 |
| --- | --- | --- |
| host 侧等大 CL buffer | 存在 | 消除 |
| XRT enqueueMigrateMemObjects | 存在 | 消除 |
| unaligned extra memcpy | 真机日志出现 | 真机验证未出现 |
| O_DIRECT/512B 对齐直读 | 未启用 | 对齐时启用 |

## 7. 当前限制

- P2P/O_DIRECT 只适用于数据 offset 与 size 满足底层存储对齐的场景;
- SeaweedFS `.dat` 中 needle 数据偏移通常不是 512B 对齐,直接 O_DIRECT 读单 chunk
  需要解决区间对齐(如扇区对齐读 + 内核偏移参数);
- 尚未把 P2P agent 与 SeaweedFS `.dat` 完整端到端联调;
- 该实现仍通过 PCIe 将数据送入 FPGA,不是设备固件内部直读 NVMe。

## 8. 论文价值

- 证明“盘 → FPGA device memory”的 P2P 通路在 SmartSSD 上可行;
- 消除 agent 侧主机缓冲与迁移,进一步逼近近存计算;
- 为 SeaweedFS needle 对齐改造和最终 NVMe 直读版本提供硬件层基础。
