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
