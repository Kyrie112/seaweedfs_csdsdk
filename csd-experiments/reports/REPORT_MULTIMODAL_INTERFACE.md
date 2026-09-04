# 多模态计算接口实现与验证

分支:`feat/multimodal-compute-interface`

## 目标

在跨 chunk 计算引擎(volume 就地计算 + filer 扇出/聚合)之上增加一层统一接口,
让同一份数据可以通过文件、对象、块三种存储接口触发计算,而计算实现只有一份。

## 实现

| 文件 | 作用 |
| --- | --- |
| `weed/server/filer_compute_api.go` | 新增 filer 统一计算入口 `/api/compute/<interface>/<resource>?compute=<op>`,支持 `file` / `object` / `block` 三种寻址方式,统一委托给现有 `proxyComputeToVolumeServer` |
| `weed/server/filer_server.go` | 在普通与只读 HTTP mux 注册 `/api/compute/` 路由 |
| `weed/s3api/s3api_compute.go` | S3 GetObject 的 compute 适配器:解析 `?x-compute=` 或 `X-SeaweedFS-Compute` 头,映射到 filer 路径后转发 |
| `weed/s3api/s3api_object_handlers.go` | 在 `GetObjectHandler` 认证后加入 compute 分支 |

统一寻址约定:

```text
GET /api/compute/file/<filer路径>?compute=<op>
GET /api/compute/object/<bucket>/<object-key>?compute=<op>
GET /api/compute/block/<volume>?compute=<op>            # 默认 backing = /blocks/<volume>
GET /api/compute/block/<volume>?compute=<op>&path=<p>   # 或显式指定 backing filer 路径
```

对象模式映射到 `<DirBucketsPath>/<bucket>/<key>`(默认 `/buckets`),
块模式把 `<volume>` 映射到原始块镜像 `/blocks/<volume>`。
所有模式最终都走同一个跨 chunk 编排,因此新增算子或聚合语义时三种接口同步生效。

## S3 对象接口调用

```bash
# 查询参数形式
curl 'http://<s3-gateway>:8333/<bucket>/<object>?x-compute=rawsum64'

# 请求头形式
curl -H 'X-SeaweedFS-Compute: rawsum64' \
     'http://<s3-gateway>:8333/<bucket>/<object>'
```

S3 网关只负责协议适配,把请求转发到 filer 后复用 volume 端计算。

## 验证(本地临时三进程集群)

数据:184,549,376 B 原始二进制,22 × 8MiB chunk,
每 8 字节 little-endian uint64 取值 `i % 1000`,CPU 参考和 `11522691456`。
三个命名空间均上传同一文件(`/dataset`、`/buckets/test`、`/blocks/vol0`),
S3 bucket `test` 内对象 `rawsum.bin`。

| 接口 | 调用 | HTTP | 结果 |
| --- | --- | --- | --- |
| 文件(原生) | `GET /dataset/rawsum.bin?compute=rawsum64` | 200 | 11522691456 |
| 文件(统一) | `GET /api/compute/file/dataset/rawsum.bin?compute=rawsum64` | 200 | 11522691456 |
| 对象(统一) | `GET /api/compute/object/test/rawsum.bin?compute=rawsum64` | 200 | 11522691456 |
| 对象(S3) | `GET /test/rawsum.bin?x-compute=rawsum64` | 200 | 11522691456 |
| 块(统一) | `GET /api/compute/block/vol0?compute=rawsum64` | 200 | 11522691456 |

filer 日志确认每次请求均进入跨 chunk 路径:

```text
compute API "object" "test/rawsum.bin" -> /buckets/test/rawsum.bin (operation "rawsum64")
compute "rawsum64" across 22 chunks of rawsum.bin
```

## 边界与后续

- 当前 `block` 模式是“块卷 = filer 中的原始块镜像文件”的逻辑模型,
  尚未对接真实 iSCSI / NVMe-oF 数据面;
- FUSE 虚拟计算路径(如 `/.compute/<op>/<path>`)尚未实现,属于后续 FS 协议适配;
- S3 版本化对象暂不支持 compute,请求会被忽略或仅计算当前对象;
- 生产环境启用 filer HTTP 认证时,S3 网关到 filer 的内部转发需要与 filer 的认证方式对齐。
