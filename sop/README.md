# 可计算存储接口 SOP

本目录为 SeaweedFS 多存储接口的“标准操作流程”(Standard Operating Procedure)。

## 设计原则

普通数据面(POSIX 文件读/写/删、S3 数据读写、块镜像读写)语义保持不动;
计算能力通过**独立的上层计算接口**提供,调用方用“存储标识 + 算子”触发,
不再把普通读路径改造成计算入口。

## 文档索引

| 文档 | 适用接口 | 计算入口 |
| --- | --- | --- |
| [SOP-01-FS.md](SOP-01-FS.md) | 文件存储 | filer HTTP `?compute=` 与 `/api/compute/file/...` |
| [SOP-02-S3-Object.md](SOP-02-S3-Object.md) | 对象存储 | S3 `?x-compute=` / 请求头,以及 `/api/compute/object/...` |
| [SOP-03-Block.md](SOP-03-Block.md) | 块存储(逻辑块卷) | `/api/compute/block/...` |
| [SOP-04-Unified-API.md](SOP-04-Unified-API.md) | 统一计算入口 | `/api/compute/{file\|object\|block}/...` |

## 通用占位符

| 占位符 | 含义 |
| --- | --- |
| `<filer-url>` | filer HTTP 服务地址 |
| `<s3-url>` | S3 网关地址 |
| `<local-file>` | 本地待上传文件 |
| `<filer-dir>` | filer 目标目录 |
| `<filename>` | 文件/对象/块镜像名称 |
| `<bucket-name>` | S3 bucket |
| `<object-key>` | S3 对象 key |
| `<block-volume-name>` | 逻辑块卷名 |
| `<operation>` | 已部署的计算算子 |
| `<expected-result>` | 调用方已知的期望结果 |

## 前置条件(所有 SOP 通用)

1. 使用包含多模态计算接口的分支版本。
2. filer / master / volume 已启动,volume 开启:

   ```text
   -volume.compute.enabled=true
   -volume.compute.dir=<volume计算脚本目录>
   -volume.compute.timeout=<超时时间>
   -volume.compute.maxOutputMB=<最大输出MB>
   ```

3. 计算算子脚本已部署到全部相关 volume 节点。
4. 调用方按需准备待处理数据与期望结果。
