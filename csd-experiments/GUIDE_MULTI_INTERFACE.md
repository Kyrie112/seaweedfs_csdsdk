# SeaweedFS 多存储接口使用指南

本文档整理 SeaweedFS(本仓库 `feat/multimodal-compute-interface` 分支)三种存储接口
的常用操作:

- 文件存储(File/FS):filer HTTP 与 FUSE
- 对象存储(Object/S3):S3 兼容接口
- 块存储(Block):逻辑块卷(原始块镜像文件)

对每种接口,统一说明上传、普通读取、修改、删除,以及可计算接口的调用方式。

## 0. 占位符约定与算子说明

命令中的占位符表示用户实际环境中的值,使用前请替换为对应内容:

| 占位符 | 含义 |
| --- | --- |
| `<filer-url>` | filer HTTP 服务地址 |
| `<s3-url>` | S3 网关地址 |
| `<local-file>` | 本地待上传文件路径 |
| `<filer-dir>` | 文件在 filer 中的目标目录 |
| `<filename>` | 文件/对象/块镜像名称 |
| `<new-local-file>` | 用于覆盖的本地新文件路径 |
| `<output-file>` | 本地输出文件路径 |
| `<mount-point>` | 本地 FUSE 挂载目录 |
| `<bucket-name>` | S3 bucket 名称 |
| `<object-key>` | S3 对象 key |
| `<block-volume-name>` | 逻辑块卷名称 |
| `<volume-alias>` | 调用方自定义的卷别名 |
| `<new-object-key>` | 复制后的新对象 key |
| `<operation>` | 计算算子名称(部署在 volume 节点) |
| `<start>-<end>` | Range 字节区间,如 `0-1048575` |

计算算子部署在 volume 节点脚本目录(例如名为 `<operation>.sh` 的脚本),并通过以下参数启用:

```text
-volume.compute.enabled=true
-volume.compute.dir=<volume计算脚本目录>
-volume.compute.timeout=<超时时间>
-volume.compute.maxOutputMB=<最大输出MB>
```

文件超过 filer `-maxMB` 后会切成多个 chunk;当前分支的跨 chunk 计算会自动扇出并在 filer 汇总。

## 1. 文件存储接口

文件接口可以通过 filer HTTP 直接访问,也可以把 filer 挂载为本地目录(FUSE)后使用标准文件命令。

### 1.1 filer HTTP

```bash
FILER=<filer-url>
```

| 操作 | 命令 |
| --- | --- |
| 上传 | `curl -F file=@<local-file> "$FILER/<filer-dir>/<filename>"` |
| 覆盖 | `curl -F file=@<new-local-file> "$FILER/<filer-dir>/<filename>"` |
| 读取 | `curl "$FILER/<filer-dir>/<filename>" -o <output-file>` |
| 范围读取 | `curl -H 'Range: bytes=<start>-<end>' "$FILER/<filer-dir>/<filename>" -o <output-file>` |
| 删除 | `curl -X DELETE "$FILER/<filer-dir>/<filename>"` |
| 计算 | `curl -s "$FILER/<filer-dir>/<filename>?compute=<operation>"` |

查看文件元数据与 chunk 数量:

```bash
curl "$FILER/<filer-dir>/<filename>?metadata=true&resolveManifest=true"
```

### 1.2 FUSE 挂载

先挂载:

```bash
mkdir -p <mount-point>
weed mount -dir=<mount-point> -filer=<filer-host>:<filer-port>
```

挂载后即可使用标准 POSIX 命令:

| 操作 | 命令 |
| --- | --- |
| 上传 | `cp <local-file> <mount-point>/<filer-dir>/<filename>` |
| 读取 | `cat <mount-point>/<filer-dir>/<filename> > <output-file>` |
| 修改 | `cp <new-local-file> <mount-point>/<filer-dir>/<filename>`,或追加写入 |
| 删除 | `rm <mount-point>/<filer-dir>/<filename>` |
| 计算 | FUSE 虚拟计算路径暂未实现,请使用 filer HTTP `?compute=` |

### 1.3 统一计算 API(文件寻址)

```bash
curl -s "$FILER/api/compute/file/<filer-dir>/<filename>?compute=<operation>"
```

## 2. 对象存储接口(S3)

filer 进程可用 `-s3` 启动内嵌 S3 网关,或单独运行 `weed s3`。
标准 S3 客户端通过 AWS 兼容 endpoint 访问;bucket/object 在 filer 内映射为 buckets 目录下的文件路径。

```bash
S3=<s3-url>
```

### 2.1 上传

创建 bucket 并上传:

```bash
aws --endpoint-url "$S3" s3api create-bucket --bucket <bucket-name>
aws --endpoint-url "$S3" s3 cp <local-file> s3://<bucket-name>/<object-key>
```

未配置 AWS 凭证时可临时使用匿名 curl 上传(需允许匿名写):

```bash
curl -X PUT --data-binary @<local-file> \
  -H 'Content-Type: application/octet-stream' \
  "$S3/<bucket-name>/<object-key>"
```

大对象建议直接使用 `aws s3 cp`,内部自动走分段上传。

### 2.2 普通读取

```bash
aws --endpoint-url "$S3" s3api get-object \
  --bucket <bucket-name> --key <object-key> \
  <output-file>

# 等价 HTTP
curl "$S3/<bucket-name>/<object-key>" -o <output-file>
```

### 2.3 修改与删除

对象存储语义下“修改”通常是整对象覆盖,或复制到新 key:

```bash
# 覆盖原对象
aws --endpoint-url "$S3" s3 cp <new-local-file> s3://<bucket-name>/<object-key>

# 复制为新对象
aws --endpoint-url "$S3" s3 cp s3://<bucket-name>/<object-key> s3://<bucket-name>/<new-object-key>

# 删除
aws --endpoint-url "$S3" s3 rm s3://<bucket-name>/<object-key>
```

### 2.4 可计算接口(对象寻址)

S3 GET 增加自定义参数 `x-compute` 或请求头 `X-SeaweedFS-Compute`,即可触发计算:

```bash
# 查询参数形式
curl -s "$S3/<bucket-name>/<object-key>?x-compute=<operation>"

# 请求头形式
curl -s -H 'X-SeaweedFS-Compute: <operation>' \
  "$S3/<bucket-name>/<object-key>"
```

需要 S3 签名时使用支持 AWS SigV4 的 curl:

```bash
curl --aws-sigv4 'aws:amz:<region>:s3' \
  --user "<access-key>:<secret-key>" \
  "$S3/<bucket-name>/<object-key>?x-compute=<operation>"
```

说明:

- `x-compute` 是自定义扩展参数,标准 `aws s3api get-object` 没有该参数,需要
  curl/预签名/自定义客户端携带并签名;
- 返回 body 是算子结果,不是对象数据;
- 计算继承 S3 GET 的读权限校验。

不启动 S3 网关时,可对 buckets 目录直接使用统一对象 API:

```bash
curl -s "$FILER/api/compute/object/<bucket-name>/<object-key>?compute=<operation>"
```

## 3. 块存储接口

当前仓库没有 iSCSI / NVMe-oF 数据面。原型采用逻辑块卷模型:
每个块卷是一个定长原始块镜像文件,按约定存放在 filer 的块镜像目录下。
未来接入真实块数据面后,统一计算 API 的块寻址仍可复用。

```bash
BLOCK=<filer-url>
```

### 3.1 上传(创建/覆盖块镜像)

```bash
curl -F file=@<local-file> "$BLOCK/blocks/<block-volume-name>"
```

### 3.2 普通读取

```bash
# 整块读取
curl "$BLOCK/blocks/<block-volume-name>" -o <output-file>

# 按字节区间读取(模拟块读)
curl -H 'Range: bytes=<start>-<end>' "$BLOCK/blocks/<block-volume-name>" -o <output-file>
```

### 3.3 修改

当前 HTTP 原型以整块覆盖为主:

```bash
curl -F file=@<new-local-file> "$BLOCK/blocks/<block-volume-name>"
```

块级随机写、iSCSI/NVMe 协议读写属于后续数据面工作。

### 3.4 删除

```bash
curl -X DELETE "$BLOCK/blocks/<block-volume-name>"
```

### 3.5 可计算接口(块寻址)

```bash
# 默认映射块镜像目录
curl -s "$BLOCK/api/compute/block/<block-volume-name>?compute=<operation>"

# 显式指定 backing 文件路径
curl -s "$BLOCK/api/compute/block/<volume-alias>?compute=<operation>&path=/blocks/<block-volume-name>"
```

## 4. 统一计算接口速查

| 存储接口 | 调用 |
| --- | --- |
| 文件(HTTP) | `GET <filer-url>/<filer-dir>/<filename>?compute=<operation>` |
| 文件(统一) | `GET <filer-url>/api/compute/file/<filer-dir>/<filename>?compute=<operation>` |
| 对象(统一) | `GET <filer-url>/api/compute/object/<bucket-name>/<object-key>?compute=<operation>` |
| 对象(S3) | `GET <s3-url>/<bucket-name>/<object-key>?x-compute=<operation>` |
| 块(统一) | `GET <filer-url>/api/compute/block/<block-volume-name>?compute=<operation>` |

跨 chunk 计算要求每 chunk 结果可合并(如数值求和类算子,由 filer 相加);
文本/顺序相关算子在多 chunk 场景目前会明确报错,不会静默给出错误结果。

## 5. 验证说明

在本地集群中以一个被切分为多个 chunk 的原始数据对象、数值求和算子验证,
文件 HTTP、统一文件 API、统一对象 API、S3 `x-compute`、统一块 API 五类入口均返回同一正确数值,
filer 日志显示算子跨多个 chunk 执行并完成汇总。具体算子与测试数据由部署环境决定,可按上表替换占位符复现。

## 6. 当前限制

- FUSE 上的计算虚拟路径尚未实现;
- 块接口是逻辑块卷模型,尚未对接 iSCSI/NVMe-oF;
- S3 版本化对象暂不按 `versionId` 路由计算;
- S3 网关转发到 filer 的内部请求在启用 filer HTTP 认证时需要对齐认证方式。
