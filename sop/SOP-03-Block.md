# SOP-03:块存储(逻辑块卷)计算调用

## 1. 目的

对逻辑块卷执行计算下沉。

当前实现使用“块卷 = filer 中原始块镜像文件”的逻辑模型:

- 块镜像存于 filer 块镜像目录;
- 计算通过统一计算 API 的块寻址触发;
- 真实 iSCSI / NVMe-oF 数据面接入后,计算入口保持一致。

## 2. 适用场景

- 把定长原始数据作为块卷;
- 需要对整卷执行扫描/聚合类算子;
- 作为 CSD / 近存计算的前置原型。

## 3. 前置条件

见 [README.md](README.md) 通用前置条件。

## 4. 块卷 CRUD(增删改查)

设:

```bash
FILER=<filer-url>
```

### 4.1 增(Create)

```bash
curl -F file=@<local-file> "$FILER/blocks/<block-volume-name>"
```

### 4.2 删(Delete)

```bash
curl -X DELETE "$FILER/blocks/<block-volume-name>"
```

### 4.3 改(Update)

当前 HTTP 原型以整块覆盖为主:

```bash
curl -F file=@<new-local-file> "$FILER/blocks/<block-volume-name>"
```

块级随机写、iSCSI/NVMe 协议读写属于后续数据面工作。

### 4.4 查(Read/Query)

```bash
# 整卷读取
curl "$FILER/blocks/<block-volume-name>" -o <output-file>

# 按字节区间读取(模拟块读)
curl -H 'Range: bytes=<start>-<end>' "$FILER/blocks/<block-volume-name>" -o <output-file>

# 查看元数据与 chunk
curl "$FILER/blocks/<block-volume-name>?metadata=true&resolveManifest=true"

# 列出块镜像目录
curl -H 'Accept: application/json' "$FILER/blocks/"
```

## 5. 计算调用

### 方式 A:默认块镜像目录映射

```bash
curl -s "$FILER/api/compute/block/<block-volume-name>?compute=<operation>"
```

默认映射到 `/blocks/<block-volume-name>`。

### 方式 B:显式指定 backing 文件

```bash
curl -s \
  "$FILER/api/compute/block/<volume-alias>?compute=<operation>&path=/blocks/<block-volume-name>"
```

## 6. 结果验证

- 成功响应为 HTTP 200,body 为算子结果;
- 与独立计算的 `<expected-result>` 比较;
- 对多 chunk 块镜像,filer 日志应显示跨 chunk 执行。

## 7. 错误处理

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| HTTP 404 | 块镜像不存在 | 检查 `/blocks/` 下文件 |
| HTTP 400 | 路径参数错误或算子不可合并 | 检查参数与算子类型 |
| HTTP 403 | volume 未开启计算 | 检查计算开关 |
| HTTP 500 | 脚本执行失败 | 查看 volume 日志 |

## 8. 注意

- 当前为逻辑块卷模型,尚未实现真实 iSCSI/NVMe 协议;
- 块卷的普通读/写仍是文件语义,计算是独立调用入口。
