# SOP-02:对象存储(S3)计算调用

## 1. 目的

通过标准 S3 对象接口访问可计算存储:

- 上传/读取/覆盖/删除对象仍使用 S3 语义;
- 计算通过 `GET Object` 的自定义扩展参数触发,不改动普通对象读取。

## 2. 适用场景

- 用户使用 AWS 兼容 S3 客户端;
- 需要对 bucket 内的对象执行计算下沉;
- 需要保留 S3 的读写权限与签名语义。

## 3. 前置条件

- 见 [README.md](README.md) 通用前置条件;
- S3 网关已启动(内嵌 filer `-s3`,或独立 `weed s3`);
- 调用方具备该对象的读权限。

## 4. 对象 CRUD(增删改查)

```bash
S3=<s3-url>
```

### 4.1 增(Create)

```bash
# 创建 bucket
aws --endpoint-url "$S3" s3api create-bucket --bucket <bucket-name>

# 上传对象
aws --endpoint-url "$S3" s3 cp <local-file> s3://<bucket-name>/<object-key>
```

大对象由 `aws s3 cp` 自动分段上传。

### 4.2 删(Delete)

```bash
# 删除对象
aws --endpoint-url "$S3" s3 rm s3://<bucket-name>/<object-key>

# 删除空 bucket
aws --endpoint-url "$S3" s3api delete-bucket --bucket <bucket-name>
```

### 4.3 改(Update)

对象存储“修改”通常为整对象覆盖或复制:

```bash
# 覆盖原对象
aws --endpoint-url "$S3" s3 cp <new-local-file> s3://<bucket-name>/<object-key>

# 复制为新对象
aws --endpoint-url "$S3" s3 cp s3://<bucket-name>/<object-key> s3://<bucket-name>/<new-object-key>
```

### 4.4 查(Read/Query)

```bash
# 读取对象
aws --endpoint-url "$S3" s3api get-object \
  --bucket <bucket-name> --key <object-key> <output-file>

# 读取对象元数据(HEAD)
aws --endpoint-url "$S3" s3api head-object \
  --bucket <bucket-name> --key <object-key>

# 列出对象
aws --endpoint-url "$S3" s3 ls s3://<bucket-name>/

# 按前缀列出对象
aws --endpoint-url "$S3" s3 ls s3://<bucket-name>/<prefix>/
```

未配置凭证时可使用 curl:

```bash
# 匿名读
curl "$S3/<bucket-name>/<object-key>" -o <output-file>

# 匿名删除
curl -X DELETE "$S3/<bucket-name>/<object-key>"
```

## 5. 计算调用

### 方式 A:S3 查询参数 `?x-compute=`

```bash
curl -s "$S3/<bucket-name>/<object-key>?x-compute=<operation>"
```

### 方式 B:S3 请求头 `X-SeaweedFS-Compute`

```bash
curl -s \
  -H 'X-SeaweedFS-Compute: <operation>' \
  "$S3/<bucket-name>/<object-key>"
```

### 方式 C:需要 S3 签名时

```bash
curl --aws-sigv4 'aws:amz:<region>:s3' \
  --user "<access-key>:<secret-key>" \
  "$S3/<bucket-name>/<object-key>?x-compute=<operation>"
```

### 方式 D:filer 统一对象 API(不经过 S3 网关时)

```bash
FILER=<filer-url>
curl -s "$FILER/api/compute/object/<bucket-name>/<object-key>?compute=<operation>"
```

## 6. 结果验证

- 成功响应为 HTTP 200,body 为算子结果;
- 与独立计算的 `<expected-result>` 比较;
- filer 日志显示对象底层按 chunk 执行:

  ```text
  compute "<operation>" across <chunk-count> chunks of <object-key>
  ```

## 7. 错误处理

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| S3 403 | 无读权限 / 签名错误 | 检查 IAM 权限与签名 |
| S3 404 | 对象不存在 | 检查 bucket/key |
| 内部错误 | filer/volume 计算失败 | 查看 filer 与 volume 日志 |
| 标准 SDK 无法传参 | `x-compute` 是自定义扩展参数 | 改用 curl/预签名/自定义客户端 |

## 8. 注意

- 普通 `GET Object` 仍返回对象数据,不会触发计算;
- 计算触发后返回算子结果而非对象数据;
- 版本化对象的按版本计算暂未支持。
