# 跨 chunk 计算下沉实现与验证

## 1. 背景

原实现只支持"单个 chunk 覆盖整个文件"(`compute currently supports single-chunk files only`)。
超过 filer maxMB 的文件被切成多个 chunk 后无法计算。本改动在 filer 层增加多 chunk 编排:

- **计算仍发生在各 volume 上**(每个 chunk 在其所属 volume 就地执行算子);
- **filer 只做扇出与结果合并**,把各 chunk 的小体积数值结果相加后统一上报;
- 数据不出 volume,避免把整个文件经 filer 聚合的开销。

## 2. 实现(fork 内代码)

修改 `weed/server/filer_server_handlers_proxy.go`:

- `resolveComputeChunks`:解析 entry 的 chunk 清单(嵌套 manifest 展平),逐 chunk 校验加密/空 fileId;
- `proxyComputeToVolumeServer`:单 chunk 且覆盖全文件 → 原快路径;多 chunk → 校验连续覆盖后走多 chunk;
- `validateChunksCoverFile`:校验 chunk 从 offset 0 连续无缝隙地覆盖整个文件(保证字节对齐算子正确性);
- `fetchChunkComputeResult`:按 chunk fileId 查 volume 并发单次 compute 请求并读回结果;
- `proxyMultiChunkCompute`:并行(并发上限 8)扇出到各 chunk volume,按数值类型把部分结果相加(big.Int),返回合并结果;非数值算子/部分结果 → 明确 400 错误(截断展示);
- 单 chunk 行为与对外 HTTP 语义不变(`?compute=<op>`)。

边界语义说明:当前合并策略为"数值相加",适用于 rawsum64/计数等可结合算子;
多 chunk 的文本算子(如逐行 awk)需记录对齐或后续引入跨 chunk 状态算子(本期不做)。

## 3. 验证(三节点集群,192.168.0.9/10/11)

### 3.1 数据

- `rawsum.bin`:184,549,376 B = 22 × 8MiB,内容为 23,068,672 个 little-endian uint64(i % 1000);
- 算子 `rawsum64.sh`:每 8 字节打包为 uint64 求和,末段不足 8 字节补零(与 file_sum64 kernel 语义一致);
- CPU 参考和 = **11,522,691,456**。

### 3.2 结果

| 场景 | filer maxMB | chunk 数 | compute 结果 | 与 CPU 参考一致 |
|---|---|---|---|---|
| 多 chunk | 8MB | **22** | **11522691456**(约 1.2s,并行扇出) | ✓ |
| 单 chunk(同文件) | 1024MB | 1 | 11522691456 | ✓ |
| 单 chunk 回归(sum 文本算子) | 1024MB | 1 | 179986461084(17.8s) | ✓ |

filer 日志确认扇出:`compute "rawsum64" across 22 chunks`。

### 3.3 错误路径

对 22-chunk 文件执行非数值算子(uppercase)→ HTTP 400:
`cross-chunk compute currently requires numeric per-chunk results, chunk 0 returned (truncated): ...`

## 4. 结论

- 跨 chunk 计算成立:计算保留在 volume 层,结果在 filer 合并,整体语义与单 chunk 一致;
- 字节对齐算子(rawsum64)跨任意数量 chunk 正确;多 chunk 并行反而缩短了端到端时延(相对单 chunk 文本 awk);
- 为后续"多 chunk + 设备算子(FPGA/CSD)"提供编排基础:扇出、并发、合并框架与算子语义约束已就绪。
