# SmartSSD CSD Compute Module

分支:`feat/csd-native-compute`

本目录是 SeaweedFS 仓库内的设备侧计算模块(原独立仓库
`smartssd_compute_module`,已通过 `git subtree` 并入,历史完整保留)。
SeaweedFS volume 只下发 `{data_file, offset, size, operation}`,
本模块在 SmartSSD 设备上常驻运行,把 volume `.dat` 的精确数据区间送入
FPGA 计算内核,返回算子结果;整个过程不经过 SeaweedFS 的临时文件/脚本路径。

## 接口

```text
POST /v1/compute
Content-Type: application/json

{
  "operation": "rawsum64",
  "data_file": "/var/lib/seaweedfs/2.dat",
  "offset": 255348,
  "size": 8388608
}
```

成功响应:

```json
{"result":"123456789"}
```

失败响应:

```json
{"error":"description"}
```

## 构建(需 Xilinx XRT + Vitis 环境)

```bash
source /opt/xilinx/xrt/setup.sh
make
```

不需要 XRT 的检查(可在普通工作站 / CI 上运行):

```bash
make test          # 主机侧参考模型:对齐窗口与 v1/v2 求和语义的等价性
make syntax-check  # 用 tools/cl_stub 的桩头文件对 agent 做类型检查
```

## 运行

```bash
./build/csd_agent \
  --xclbin=<file_sum64.xclbin> \
  --listen=127.0.0.1:18090
```

SeaweedFS volume 侧:

```text
-volume.compute.csd.enabled=true
-volume.compute.csd.endpoint=http://127.0.0.1:18090
-volume.compute.csd.timeout=60s
```

## 当前限制

- 数据必须以未压缩 needle 存储(压缩 / chunk manifest 会由 SeaweedFS 自动回退 CPU);
- 设备端只实现 `rawsum64` / `file_sum64` 同一求和内核;
- v2 内核需要重新合成 xclbin(见下节);未重新合成的节点会自动退回 v1 语义,
  结果依然正确,只是拿不到 needle 场景的 O_DIRECT。

## .9(SmartSSD 实机)编译与验证记录

执行位置:`192.168.0.9`,XRT 2.14.354,Vitis 2022.2,设备 `xilinx_u2_gen3x4_xdma_gc_2_202110_1`。

1. 安装缺失的 OpenCL C++ 头文件:`apt-get install opencl-clhpp-headers opencl-headers`
2. 安装 Vitis 平台包:
   `/home/dess/workspace/dess_smartssd_setups/Xilinx/xilinx-u2-gen3x4-xdma-gc-2-202110-1-dev_1-3311104_all.deb`
3. 编译 host agent:

   ```bash
   cd smartssd_compute_module
   source /opt/xilinx/xrt/setup.sh
   make
   # build/csd_agent
   ```

4. 编译 file_sum64 内核并用 v++ 硬件链接生成 xclbin(约 2 小时):

   ```bash
   /home/dess/workspace/dess_smartssd_setups/Vitis/2022.2/bin/v++ \\
     -t hw --platform /opt/xilinx/platforms/xilinx_u2_gen3x4_xdma_gc_2_202110_1/xilinx_u2_gen3x4_xdma_gc_2_202110_1.xpfm \\
     -c -k file_sum64 --temp_dir _x.hw -o _x.hw/file_sum64.xo src/file_sum64_kernel.cpp
   /home/dess/workspace/dess_smartssd_setups/Vitis/2022.2/bin/v++ \\
     -t hw --platform <同上> --link --temp_dir _x.hw \\
     -o _x.hw/file_sum64.link.xclbin _x.hw/file_sum64.xo
   cp _x.hw/file_sum64.link.xclbin file_sum64.xclbin
   ```

5. 真实设备功能验证:

   ```bash
   ./build/csd_agent --xclbin=file_sum64.xclbin --listen=127.0.0.1:18090 &
   curl -s -X POST -H 'Content-Type: application/json' -d @request.json \
     http://127.0.0.1:18090/v1/compute
   ```

   测试数据:8 个 little-endian uint64 `[3,10,17,24,31,38,45,52]`,
   期望 `220`,实际 agent 返回 `220`;非零 offset 场景同样返回 `220`。

## P2P 版本(V3)

`src/csd_agent.cpp` 使用 `XCL_MEM_EXT_P2P_BUFFER` 创建输入缓冲区,
`pread` 直接写入 FPGA device memory 的映射,不再经过 host CL buffer 与
XRT migrate;offset/size 满足 512B 对齐时启用 `O_DIRECT`。

真机验证:

- 64B 非对齐请求:offset 0 → 54,offset 32 → 220;
- 512B O_DIRECT+P2P 请求:期望 1760,返回 1760;
- 日志不再出现 `unaligned host pointer ... extra memcpy`。

## 对齐读与 v2 内核(V6)

### 问题

SeaweedFS needle 的数据区起点是 `needleOffset + NeedleHeaderSize + DataSizeSize`,
即固定 `+20`,而 needle 在 `.dat` 中没有填充对齐。此前 agent 用
`offset % 512 == 0 && size % 512 == 0` 判断能否 `O_DIRECT`,该条件在真实业务请求下
几乎恒为假,所以实际走的是普通 `pread`(page cache + 主机内存),
“P2P + O_DIRECT”只在人工构造对齐请求时才成立。

### 做法

1. SeaweedFS 在区间描述符中额外下发对齐窗口:
   `read_offset`(payload 起点向下对齐)、`read_size`(覆盖 payload 的对齐长度)、
   `payload_shift`(`offset - read_offset`);
2. agent 用 `read_offset/read_size` 打开 `O_DIRECT` 读入 P2P buffer;
3. 由于整段左移会改变 uint64 打包权重,内核升级为 v2,新增
   `payload_shift` / `payload_size` 两个参数,使打包从 payload 首字节起算,
   结果与“从 payload 首地址打包”逐字节一致;
4. 兼容性:agent 启动时用 `CL_KERNEL_NUM_ARGS` 探测内核版本,3 参数按 v1 语义、
   5 参数按 v2 语义;遇到其它参数个数直接报错而不是算错。

### 重新合成 xclbin

```bash
/home/dess/workspace/dess_smartssd_setups/Vitis/2022.2/bin/v++ \
  -t hw --platform /opt/xilinx/platforms/xilinx_u2_gen3x4_xdma_gc_2_202110_1/xilinx_u2_gen3x4_xdma_gc_2_202110_1.xpfm \
  -c -k file_sum64 --temp_dir _x.hw -o _x.hw/file_sum64.xo src/file_sum64_kernel.cpp
/home/dess/workspace/dess_smartssd_setups/Vitis/2022.2/bin/v++ \
  -t hw --platform <同上> --link --temp_dir _x.hw \
  -o _x.hw/file_sum64.link.xclbin _x.hw/file_sum64.xo
cp _x.hw/file_sum64.link.xclbin file_sum64.xclbin
```

### 读取完整性与可观测性

- `read_fully_at` 循环读满并在读不满时返回短计数,调用方据此报错,
  不再出现“短读被当成成功”的静默错误;
- agent 每次请求输出一行审计日志:
  `window=[start,end) shift=… kernel=v1|v2 direct=yes|no read_ms=… kernel_ms=…`,
  并累计 `direct_hits / direct_misses / short_reads`,可直接用于证明 O_DIRECT 是否生效;
- 响应体新增 `read_ms` / `kernel_ms` / `direct` 字段,
  SeaweedFS volume 侧转成 `X-CSD-Read-Ms` 等响应头,供 filer 汇总分阶段耗时。

## 自适应数据通路选择(V7)

冷数据下 O_DIRECT 更快(省掉「磁盘→页缓存→缓冲区」的第二次拷贝),
但数据已在页缓存时 O_DIRECT 明显更慢——实测热场景 168ms vs 27ms。
因此 agent 支持按请求感知缓存并选择路径:

```bash
./build/csd_agent --xclbin=file_sum64.xclbin --listen=127.0.0.1:18090 \
  --cache-policy=auto      # auto(默认) | direct | buffered
```

- `auto`:先用 `preadv2(RWF_NOWAIT)` 尝试读对齐窗口。**完整成功说明数据已在页缓存,
  而且这次 syscall 本身就是数据搬运**,没有额外开销;`EAGAIN` 或短读(部分命中)
  则丢弃结果、回退到 O_DIRECT 整窗重读。预测失败只会退化为原路径,不会让请求失败。
- `direct`:始终 O_DIRECT(修复后的原行为)。
- `buffered`:始终走页缓存读。

计数器 `cache_hits / cache_partial / cache_misses / probe_errors` 随审计日志输出,
读路径经响应字段 `path` 透传到 SeaweedFS 的 `X-CSD-Path` / `X-Compute-Cache-Hits`。

**注意(实测结论)**:O_DIRECT 读不填充页缓存,所以如果 compute 路径是文件唯一的
读者,`auto` 会一直命中失败、退化成 `direct`。自适应的收益依赖数据也被普通读路径
(FUSE / S3 / 副本复制)访问过。若负载是反复计算同一区间且只经过 compute 路径,
需要额外机制(首次访问「种」缓存、或按热度周期性回退到缓冲读)并权衡缓存污染。

## 近存以图搜图算子(V8)

`src/image_topk.h` 实现了近存 Top-K 相似检索算子:图库是若干 64×64 灰度缩略图
(4096 字节/张)的拼接,设备侧一次流式扫描即可得到每张图的整数描述子
(64 桶灰度直方图 + 4 个象限和),并以 L1 距离维护片内 Top-K。

```text
POST /v1/compute
{
  "operation": "img_topk",
  "data_file": "/var/lib/seaweedfs/3.dat",
  "offset": ..., "size": ...,
  "read_offset": ..., "read_size": ..., "payload_shift": ...,
  "params": {"k":"5", "rec":"4096", "q":"<base64 描述子>", "base_off":"4194304"}
}
->  1594:2569,2042:2584,4066:2592,698:2628,1306:2636
```

要点:

- `q` 是**查询描述子**而非查询图像:原始查询图像不出客户端;
- `base_off` 是该块在文件中的**逻辑起点**(由 filer 下发),
  用以把记录索引与 `.dat` 的物理偏移解耦——混用二者会静默产生错误结果;
- 跨块结果由 filer 做"局部 Top-K + 全局归并",依据是
  "全局 Top-K 包含于各块局部 Top-K 之并";
- `src/imgtopk_kernel.cpp` 是待综合的 HLS 内核;在完成综合前,agent 以主机侧
  软件实现同一算子(`path` 字段会标注 `+hosttopk`),数据通路与协议完全一致;
- `make test` 覆盖描述子定义、跨块 Top-K 等价性与越界记录跳过(共 373 项检查)。
