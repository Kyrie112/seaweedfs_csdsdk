# SOP-01:文件存储(FS)计算调用

## 1. 目的

对文件存储接口中的数据执行计算下沉。

本 SOP 保持普通 POSIX / filer HTTP 文件语义不变:

- 普通读/写/删仍按文件接口执行;
- 计算通过独立的上层接口以“文件路径 + 算子”的方式触发。

## 2. 适用场景

- 数据通过 filer HTTP 或 FUSE 存放;
- 需要对单个文件执行已部署的聚合/转换类算子(`<operation>`);
- 文件可能被切成多个 chunk,需要跨 chunk 计算。

## 3. 前置条件

见 [README.md](README.md) 通用前置条件。

## 4. 文件 CRUD(增删改查)

文件存储的数据操作保持标准文件语义,下面按两种使用方式列出。

### 4.1 通过 filer HTTP

设:

```bash
FILER=<filer-url>
```

| CRUD | 操作 | 命令 |
| --- | --- | --- |
| 增 | 上传/创建文件 | `curl -F file=@<local-file> "$FILER/<filer-dir>/<filename>"` |
| 删 | 删除文件 | `curl -X DELETE "$FILER/<filer-dir>/<filename>"` |
| 改 | 覆盖/修改文件 | `curl -F file=@<new-local-file> "$FILER/<filer-dir>/<filename>"` |
| 查 | 读取文件 | `curl "$FILER/<filer-dir>/<filename>" -o <output-file>` |
| 查 | 范围读取 | `curl -H 'Range: bytes=<start>-<end>' "$FILER/<filer-dir>/<filename>" -o <output-file>` |
| 查 | 元数据与 chunk | `curl "$FILER/<filer-dir>/<filename>?metadata=true&resolveManifest=true"` |
| 查 | 目录列表 | `curl -H 'Accept: application/json' "$FILER/<filer-dir>/"` |
| 查 | 分页目录列表 | `curl -H 'Accept: application/json' "$FILER/<filer-dir>/?limit=<limit>&lastFileName=<last-file-name>&namePattern=<pattern>"` |

### 4.2 通过 FUSE 挂载

挂载后使用标准 POSIX 命令:

```bash
mkdir -p <mount-point>
weed mount -dir=<mount-point> -filer=<filer-host>:<filer-port>
```

| CRUD | 操作 | 命令 |
| --- | --- | --- |
| 增 | 创建/复制 | `cp <local-file> <mount-point>/<filer-dir>/<filename>` |
| 删 | 删除 | `rm <mount-point>/<filer-dir>/<filename>` |
| 改 | 覆盖 | `cp <new-local-file> <mount-point>/<filer-dir>/<filename>` |
| 改 | 追加 | `echo <content> >> <mount-point>/<filer-dir>/<filename>` |
| 查 | 读取 | `cat <mount-point>/<filer-dir>/<filename> > <output-file>` |
| 查 | 属性 | `stat <mount-point>/<filer-dir>/<filename>` |
| 查 | 目录列表 | `ls -la <mount-point>/<filer-dir>/` |

注意:FUSE 用于普通文件 CRUD;计算不在普通读路径上触发。

## 5. 计算调用

### 方式 A:filer 原生文件路径计算

```bash
curl -s "$FILER/<filer-dir>/<filename>?compute=<operation>"
```

### 方式 B:统一计算 API(文件模式)

```bash
curl -s "$FILER/api/compute/file/<filer-dir>/<filename>?compute=<operation>"
```

### 响应

- 成功:HTTP 200,响应体为算子结果(文本);
- 单 chunk:由 volume 就地计算并直接返回;
- 多 chunk:由 filer 并发扇出到各 chunk 所在 volume,汇总后返回。

## 6. 结果验证

1. 先独立计算期望结果 `<expected-result>`(例如用 CPU 程序或脚本);
2. 调用后比较响应体与 `<expected-result>`;
3. 查看 filer 日志确认是否进入跨 chunk 路径:

   ```text
   compute "<operation>" across <chunk-count> chunks of <filename>
   ```

## 7. 错误处理

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| HTTP 400 | 算子不存在 / 不支持跨 chunk 合并 | 核对算子名称,使用可合并算子 |
| HTTP 403 | volume 未开启计算 | 检查 `-volume.compute.enabled` |
| HTTP 404 | 文件不存在 | 检查路径与上传结果 |
| HTTP 500 | volume 脚本执行失败 | 查看 volume 日志与脚本 stderr |
| 超时 | 文件过大或算子耗时超过超时时间 | 调整 `-volume.compute.timeout` |

## 8. 注意

- 文件路径上的计算入口不改变文件本身的普通读/写/删行为;
- FUSE 挂载仍用于普通数据访问,不建议把计算请求伪装成普通文件读。
