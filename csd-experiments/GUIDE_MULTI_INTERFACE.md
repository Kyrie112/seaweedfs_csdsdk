# SeaweedFS 多存储接口使用指南

本文档整理 SeaweedFS(本仓库 `feat/multimodal-compute-interface` 分支)三种存储接口
的常用操作:

- 文件存储(File/FS):filer HTTP 与 FUSE
- 对象存储(Object/S3):S3 兼容接口
- 块存储(Block):当前原型中的逻辑块卷(原始块镜像文件)

对每种接口,统一说明上传、普通读取、修改、删除,以及可计算接口的调用方式。

## 0. 环境与算子约定

实验集群(以下命令可直接替换为主机地址):

| 服务 | 地址 | 说明 |
| --- | --- | --- |
| filer HTTP | `http://192.168.0.9:8888` | 文件路径与统一计算 API |
| master | `192.168.0.9:9333` | 集群控制面 |
| volume | `.9/.10/.11:8080` | 数据节点,算子就地执行 |
| S3(需启用) | `http://<s3-host>:8333` | filer 以 `-s3` 启动后提供 |

计算算子部署在 volume 节点脚本目录(如 `/home/dess/compute_program`),
并通过以下参数启用:

```text
-volume.compute.enabled=true
-volume.compute.dir=/home/dess/compute_program
-volume.compute.timeout=300s
-volume.compute.maxOutputMB=16
```

已有算子示例:`sum`、`uppercase`、`rawsum64`。
文件超过 filer `-maxMB` 后会切成多个 chunk,当前分支的跨 chunk 计算会自动扇出并在 filer 汇总。

## 1. 文件存储接口

文件接口可以通过 filer HTTP 直接访问,也可以把 filer 挂载为本地目录(FUSE)后使用标准文件命令。

### 1.1 filer HTTP

设 `FILER=http://192.168.0.9:8888`。

| 操作 | 命令 |
| --- | --- |
| 上传/覆盖 | `curl -F file=@rawsum.bin "$FILER/dataset/rawsum.bin"` |
| 读取 | `curl "$FILER/dataset/rawsum.bin" -o rawsum.bin` |
| 修改 | 重新上传同名路径即可覆盖:`curl -F file=@rawsum_new.bin "$FILER/dataset/rawsum.bin"` |
| 删除 | `curl -X DELETE "$FILER/dataset/rawsum.bin"` |
| 计算 | `curl -s "$FILER/dataset/rawsum.bin?compute=rawsum64"` |

读取局部范围:

```bash
curl -H 'Range: bytes=0-1048575' "$FILER/dataset/rawsum.bin" -o first_1MB.bin
```

查看文件元数据与 chunk 数量:

```bash
curl "$FILER/dataset/rawsum.bin?metadata=true&resolveManifest=true"
```

### 1.2 FUSE 挂载

先挂载:

```bash
mkdir -p /mnt/seaweed
weed mount -dir=/mnt/seaweed -filer=192.168.0.9:8888
```

挂载后即可使用标准 POSIX 命令:

| 操作 | 命令 |
| --- | --- |
| 上传 | `cp rawsum.bin /mnt/seaweed/dataset/rawsum.bin` |
| 读取 | `cat /mnt/seaweed/dataset/rawsum.bin > rawsum.bin` |
| 修改 | `cp rawsum_new.bin /mnt/seaweed/dataset/rawsum.bin`,或 `echo ... >> file` 追加 |
| 删除 | `rm /mnt/seaweed/dataset/rawsum.bin` |
| 计算 | FUSE 虚拟计算路径暂未实现,请使用 filer HTTP `?compute=` |

### 1.3 统一计算 API(文件寻址)

```bash
curl -s "$FILER/api/compute/file/dataset/rawsum.bin?compute=rawsum64"
```

## 2. 对象存储接口(S3)

SeaweedFS 的 filer 进程可用 `-s3 -s3.port=8333` 启动内嵌 S3 网关,或单独运行 `weed s3`。
标准 S3 客户端通过 AWS 兼容 endpoint 访问;bucket/object 在 filer 内映射为
`<DirBucketsPath>/<bucket>/<object>`,默认 buckets 目录为 `/buckets`。

设 `S3=http://<s3-host>:8333`。

### 2.1 上传

创建 bucket 并上传:

```bash
aws --endpoint-url "$S3" s3api create-bucket --bucket test
aws --endpoint-url "$S3" s3 cp rawsum.bin s3://test/rawsum.bin
```

未配置 AWS 凭证时可临时使用匿名 curl 上传(需允许匿名写):

```bash
curl -X PUT --data-binary @rawsum.bin \
  -H 'Content-Type: application/octet-stream' \
  "$S3/test/rawsum.bin"
```

大对象建议直接使用 `aws s3 cp`,内部自动走分段上传。

### 2.2 普通读取

```bash
aws --endpoint-url "$S3" s3api get-object \
  --bucket test --key rawsum.bin \
  /tmp/rawsum.bin

# 等价 HTTP
curl "$S3/test/rawsum.bin" -o rawsum.bin
```

### 2.3 修改

对象存储语义下“修改”通常是整对象覆盖,或复制到新 key:

```bash
# 覆盖原对象
aws --endpoint-url "$S3" s3 cp rawsum_new.bin s3://test/rawsum.bin

# 复制为新对象
aws --endpoint-url "$S3" s3 cp s3://test/rawsum.bin s3://test/rawsum.copy

# 删除
aws --endpoint-url "$S3" s3 rm s3://test/rawsum.bin
```

### 2.4 可计算接口(对象寻址)

S3 GET 增加自定义参数 `x-compute` 或请求头 `X-SeaweedFS-Compute`,即可触发计算:

```bash
# 查询参数形式(已验证)
curl -s "$S3/test/rawsum.bin?x-compute=rawsum64"

# 请求头形式
curl -s -H 'X-SeaweedFS-Compute: rawsum64' \
  "$S3/test/rawsum.bin"
```

需要 S3 签名时使用支持 AWS SigV4 的 curl:

```bash
curl --aws-sigv4 'aws:amz:us-east-1:s3' \
  --user "$ACCESS_KEY:$SECRET_KEY" \
  "$S3/test/rawsum.bin?x-compute=rawsum64"
```

说明:

- `x-compute` 是自定义扩展参数,标准 `aws s3api get-object` 没有该参数,需要
  curl/预签名/自定义客户端携带并签名;
- 返回 body 是算子结果(如 `rawsum64` 的十进制和),不是对象数据;
- 计算继承 S3 GET 的读权限校验。

不启动 S3 网关时,可对 buckets 目录直接使用统一对象 API:

```bash
curl -s "$FILER/api/compute/object/test/rawsum.bin?compute=rawsum64"
```

## 3. 块存储接口

当前仓库没有 iSCSI / NVMe-oF 数据面。原型采用逻辑块卷模型:
每个块卷是一个定长原始块镜像文件,按约定存放在 filer 的 `/blocks/` 目录下。
未来接入真实块数据面后,`/api/compute/block` 仍可复用。

设 `BLOCK=http://192.168.0.9:8888`。

### 3.1 上传(创建/覆盖块镜像)

```bash
curl -F file=@vol0.img "$BLOCK/blocks/vol0"
```

### 3.2 普通读取

```bash
# 整块读取
curl "$BLOCK/blocks/vol0" -o vol0.img

# 按 LBA/字节区间读取(模拟块读)
curl -H 'Range: bytes=0-1048575' "$BLOCK/blocks/vol0" -o vol0_first_1MB.img
```

### 3.3 修改

当前 HTTP 原型以整块覆盖为主:

```bash
curl -F file=@vol0_new.img "$BLOCK/blocks/vol0"
```

块级随机写、iSCSI/NVMe 协议读写属于后续数据面工作。

### 3.4 删除

```bash
curl -X DELETE "$BLOCK/blocks/vol0"
```

### 3.5 可计算接口(块寻址)

```bash
# 默认映射 /blocks/vol0
curl -s "$BLOCK/api/compute/block/vol0?compute=rawsum64"

# 显式指定 backing 文件
curl -s "$BLOCK/api/compute/block/myvol?compute=rawsum64&path=/blocks/vol0"
```

## 4. 统一计算接口速查

| 存储接口 | 调用 | 备注 |
| --- | --- | --- |
| 文件(HTTP) | `GET /dataset/rawsum.bin?compute=rawsum64` | 原生 file 路径 |
| 文件(统一) | `GET /api/compute/file/dataset/rawsum.bin?compute=rawsum64` | 显式 file 模式 |
| 对象(统一) | `GET /api/compute/object/test/rawsum.bin?compute=rawsum64` | 映射 `/buckets/test/rawsum.bin` |
| 对象(S3) | `GET /test/rawsum.bin?x-compute=rawsum64` | S3 网关 + SigV4 |
| 块(统一) | `GET /api/compute/block/vol0?compute=rawsum64` | 映射 `/blocks/vol0` |

跨 chunk 计算只要求每 chunk 结果可合并(如 `rawsum64` 返回整数由 filer 相加);
文本类算子(如逐行 `sum`/`uppercase`)在多 chunk 场景目前会明确报 400,不会静默给错结果。

## 5. 验证样例

184,549,376 B 原始文件,22 × 8MiB chunk,`rawsum64` 正确结果为 `11522691456`。
以下五类入口在本地验证全部返回该结果:

```text
file  native  ?compute=rawsum64                  -> 11522691456
file  unified /api/compute/file/...              -> 11522691456
object unified /api/compute/object/...           -> 11522691456
object S3      ?x-compute=rawsum64               -> 11522691456
block unified  /api/compute/block/...            -> 11522691456
```

filer 日志:

```text
compute "rawsum64" across 22 chunks of rawsum.bin
```

## 6. 当前限制

- FUSE 上的计算虚拟路径尚未实现;
- 块接口是逻辑块卷模型,尚未对接 iSCSI/NVMe-oF;
- S3 版本化对象暂不按 `versionId` 路由计算;
- S3 网关转发到 filer 的内部请求在启用 filer HTTP 认证时需要对齐认证方式。
