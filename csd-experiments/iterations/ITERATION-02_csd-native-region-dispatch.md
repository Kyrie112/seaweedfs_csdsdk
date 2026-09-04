# V2:SeaweedFS CSD 原生分派 + SmartSSD agent

## 1. 目的

把 V0 中“磁盘 → 主机内存 → 临时文件 → CPU 脚本”的计算路径,
改为“volume 只下发 `.dat` 精确数据区间 → CSD agent → SmartSSD FPGA 内核”。

## 2. 时间与分支

- 时间:2026-09-03 前后
- SeaweedFS 分支:`feat/csd-native-compute`
  - 提交:`79c951dbc feat: route volume compute to CSD native engine without full-needle read`
- CSD 模块分支(独立本地仓库):`feat/csd-native-compute`
  - 提交:`96edb01` → `71b98d4` → `8b38648` → `be4f074`

## 3. 目标数据路径

```text
filer 跨 chunk 扇出
   ↓
volume server 收到 ?compute=
   ↓ 只读 needle 元数据,不整段读数据
   ↓ 得到 {data_file, data_offset, data_size}
   ↓ POST /v1/compute
   ↓
CSD agent(SmartSSD 常驻进程)
   ↓ pread .dat[offset:offset+size]
   ↓ file_sum64 内核(FPGA)
   ↓ 返回十进制结果
   ↓
filer 汇总各 chunk 结果
```

## 4. SeaweedFS 侧实现

### 4.1 needle 区间定位(不读数据主体)

- `weed/storage/volume_read.go`:
  - 新增 `NeedleComputeRegion`;
  - 从 needle map 取得物理 offset/size;
  - 调用 `ReadNeedleMeta` 只读取元数据,得到 `DataSize` 与压缩标志;
  - 返回 `DataFile + DataOffset + DataSize + Cookie`;
  - 压缩/chunk manifest/legacy 版本返回 unsupported,交给 CPU 回退。

### 4.2 读路径优先 CSD

- `weed/server/volume_server_handlers_read.go`:
  - 在 `ReadVolumeNeedle`(全量读)之前检查 CSD 是否可用;
  - 可用则调用 CSD,避免整段读入主机内存。

### 4.3 CSD agent 客户端

- `weed/server/csd_compute.go`:
  - `POST {operation,data_file,offset,size}`;
  - 响应 `{"result":"<十进制>"}`;
  - 失败或算子不受支持时回落原有脚本路径。

### 4.4 启动参数

```text
-volume.compute.csd.enabled=true
-volume.compute.csd.endpoint=http://127.0.0.1:18090
-volume.compute.csd.timeout=60s
```

## 5. CSD 模块侧实现

### 5.1 常驻 agent

- `src/csd_agent.cpp`:
  - 启动时加载一次 `file_sum64.xclbin`,复用 OpenCL context/program;
  - HTTP `POST /v1/compute`;
  - `pread` `.dat` 精确区间到 FPGA buffer;
  - 返回求和结果。

### 5.2 内核

- `src/file_sum64_kernel.cpp`:
  - 输入 `(unsigned char* file_data, uint32_t byte_count, uint64_t* sum_out)`;
  - 每 8 字节打包为 little-endian uint64 并累加,末尾不足 8 字节补零。

## 6. 编译与产物(.9 实机)

- 平台:`xilinx_u2_gen3x4_xdma_gc_2_202110_1`
- XRT:2.14.354,Vitis:2022.2
- 编译:
  - `build/csd_agent`
  - `file_sum64.xclbin`(硬件链接,约 1h54m)
- 设备:`xbutil examine` 可见 `xilinx_u2_gen3x4_xdma_gc_base_2`

## 7. 验证结果

### 7.1 SeaweedFS 分派(本地集群 + 模拟 agent)

- 随机不可压缩 16MiB 文件,2 × 8MiB chunk;
- 模拟 agent 收到 2 个 `/v1/compute` 请求,offset/size 与 `.dat` 中数据区间一致;
- 跨 chunk 结果 `19332267092931953402262659`,与 CPU 参考一致;
- 压缩文件自动回落 CPU 脚本,结果仍正确(`11522691456`),且不再新增 CSD 请求。

### 7.2 SmartSSD 真机(agent 直接请求)

- 8 个 little-endian uint64:`[3,10,17,24,31,38,45,52]`
- `POST /v1/compute`,`offset=0,size=64` → `{"result":"220"}` ✓
- `offset=32,size=64` → `{"result":"220"}` ✓

### 7.3 集成状态

- SeaweedFS ↔ CSD agent 协议已分别验证;
- 尚未在 .9 上把 SeaweedFS 新分支与真实 agent 做端到端联调。

### 7.4 数据流动逐步拆解

SeaweedFS 侧(已消除全量读取):

```text
用户 ?compute=
  → filer 解析 chunk 清单
  → volume 只读元数据定位 {.dat, offset, size}
  → POST /v1/compute
  → 等小数结果
```

CSD agent 侧(仍有一次主要数据搬运):

```text
磁盘 .dat
  → open()(未使用 O_DIRECT)
  → pread:磁盘 → OS page cache → host CL buffer
  → XRT enqueueMigrateMemObjects:host buffer → FPGA device memory
  → file_sum64 内核 AXI 读 device memory
  → 8B 结果返回
```

当前实测中 XRT 还会打印:

```text
[XRT] WARNING: unaligned host pointer detected, this leads to extra memcpy
```

说明 host 指针未满足 XRT 的固定/对齐要求时,还会先复制到内部对齐缓冲区。

### 7.5 缺陷分析

| # | 缺陷 | 位置 | 影响 |
| --- | --- | --- | --- |
| 1 | agent 仍需要一块与 chunk 等大的主机内存 | `pread` 目标 | 主机内存开销仍随 chunk 大小增长 |
| 2 | 数据经过 page cache | 普通文件 `open/pread` | 多一层内核缓冲拷贝,缺少 O_DIRECT |
| 3 | host→FPGA 迁移 | XRT migrate | 一次 PCIe 数据流 |
| 4 | host 指针未对齐触发额外 memcpy | CL buffer | 真机日志已确认 |
| 5 | 数据需先到 host 再进 FPGA | 当前架构 | 未实现盘直通/P2P |
| 6 | volume `.dat` 未验证放在 SmartSSD 本地 NVMe | 部署前提 | 若跨网络,则仍有网络传输 |
| 7 | 两段未完整端到端联调 | 集成 | 协议已对齐但未在同机验证 |

### 7.6 相比 V0/V1 的改进与剩余问题

| 对比项 | V0/V1 | V2 |
| --- | --- | --- |
| 临时文件 | 有 | 无 |
| SeaweedFS 整段读入 `n.Data` | 有 | 无(只读元数据) |
| 计算位置 | CPU 脚本 | SmartSSD FPGA |
| 数据路径 | 盘→内存→临时文件→CPU | 盘→host CL buffer→FPGA→内核 |
| 主机内存占用 | 与 chunk 等大 | 仍与 chunk 等大(在 agent 进程) |
| 盘直通 | 无 | 无(下一步 P2P) |

## 8. 当前数据流与局限

```text
.dat → pread → host CL buffer → XRT migrate → FPGA DDR → kernel
```

- 已消除:临时文件、SeaweedFS 整段主机内存读入;
- 仍存在:一次从盘到 FPGA buffer 的数据搬运,且当前为 `CL_MEM_USE_HOST_PTR`,
  可能仍有 host 侧 memcpy;
- 真机日志显示 `unaligned host pointer ... extra memcpy`,说明尚未达到 P2P 直通。

## 9. 论文价值

- 计算请求抽象为“volume 文件 + offset + size”,与文件系统无关;
- volume 只读元数据即可调度 CSD,不给计算数据制造第二次拷贝;
- SmartSSD agent 常驻、xclbin 只加载一次,具备工程可行性;
- 为后续“盘直通 FPGA”的 P2P 版本提供稳定接口。

## 10. 下一迭代

- 将 agent 改为 XRT P2P buffer(`XCL_MEM_EXT_P2P_BUFFER` + `enqueueMapBuffer`);
- 在 .9 完成 SeaweedFS 新分支 + 真实 agent 的全链路联调;
- 对比 V0/V2 端到端时延、带宽、CPU 占用。
