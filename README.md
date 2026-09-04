# SmartSSD CSD Compute Module

分支:`feat/csd-native-compute`

与 SeaweedFS 分支 `feat/csd-native-compute` 配套的 CSD 计算模块。
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

- 第一版要求数据以未压缩 needle 存储(压缩/chunk manifest 会由 SeaweedFS 自动回退 CPU);
- 当前实现把数据区间 `pread` 到 FPGA buffer(无临时文件);
  后续可升级为 XRT P2P buffer / NVMe 直读;
- 设备端支持 `rawsum64` / `file_sum64` 同一求和内核。

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
