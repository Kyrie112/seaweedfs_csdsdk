# V1:跨 chunk + 文件/对象/块多模态上层接口

## 1. 目的

在上层让用户可以用文件、对象、块三种存储接口语义触发同一套计算下沉,
同时让超过单 chunk 上限的大文件也能计算。

## 2. 时间与分支

- 时间:2026-09-02 前后
- SeaweedFS 分支:`feat/multimodal-compute-interface`

## 3. 总体架构

```text
用户
 ├── 文件寻址  GET /<path>?compute=<op>
 ├── 对象寻址  GET /bucket/key?x-compute=<op>      (S3)
 ├── 块寻址    GET /api/compute/block/<volume>
 └── 统一寻址  GET /api/compute/{file|object|block}/<resource>?compute=<op>
                    ↓
                filer 解析 chunk 清单
                    ↓
                并发扇出到各 chunk volume
                    ↓
                volume 计算 → filer 汇总
```

## 4. 实现内容

### 4.1 跨 chunk 编排

- `weed/server/filer_server_handlers_proxy.go`:
  - `resolveComputeChunks`:展平 chunk manifest,校验加密与空 fileId;
  - `validateChunksCoverFile`:校验 chunk 从 offset 0 连续覆盖整个文件;
  - `proxyMultiChunkCompute`:并发(上限 8)向各 volume 下发计算,并把各 chunk
    的整数结果在 filer 用 `big.Int` 汇总。

### 4.2 统一计算 API

- `weed/server/filer_compute_api.go`:注册
  `GET /api/compute/{file|object|block}/<resource>?compute=<op>`;
  - file → 直接 filer 路径;
  - object → bucket/object 映射到 buckets 目录;
  - block → 默认映射 `/blocks/<volume>`,也支持 `path=` 显式指定 backing 文件。

### 4.3 S3 对象接口扩展

- `weed/s3api/s3api_compute.go`:在 S3 `GetObjectHandler` 认证后,
  识别 `?x-compute=` 或 `X-SeaweedFS-Compute`,转发到 filer 计算引擎。

### 4.4 文档

- `csd-experiments/GUIDE_MULTI_INTERFACE.md`:多接口使用指南;
- `sop/`:文件、对象、块、统一 API 的 SOP。

## 5. 验证结果

本地三进程集群,184,549,376 B 文件、22 × 8MiB chunk,`rawsum64`:

| 调用入口 | 结果 |
| --- | --- |
| 文件 HTTP `?compute=rawsum64` | 11522691456 |
| 统一文件 `/api/compute/file/...` | 11522691456 |
| 统一对象 `/api/compute/object/...` | 11522691456 |
| S3 `?x-compute=rawsum64` | 11522691456 |
| 统一块 `/api/compute/block/...` | 11522691456 |

filer 日志确认 `compute "rawsum64" across 22 chunks`。

## 6. 当前限制

- 计算仍走 V0 的“脚本 + 临时文件”,没有真正下沉到 SmartSSD;
- FUSE 计算虚拟路径未实现,文件计算通过独立上层 API 触发;
- 块为逻辑块卷模型,无真实 iSCSI/NVMe-oF;
- 文本/顺序相关算子跨 chunk 时返回明确错误,不做错误聚合。

## 7. 论文价值

- 证明“保持文件/对象/块语义 + 计算请求叠加”的上层模型可行;
- 扇出、并发、聚合框架可作为后续任何底层执行引擎的复用层。

## 8. 下一迭代

V2 将底层从“脚本 + 临时文件”替换为“CSD 原生计算路径”。
