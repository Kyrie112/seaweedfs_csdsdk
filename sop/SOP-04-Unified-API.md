# SOP-04:统一计算 API

## 1. 目的

统一计算 API 是文件、对象、块三种存储寻址方式共享的计算入口。

三种寻址都汇聚到同一套 filer -> volume 编排:

```text
GET /api/compute/{file|object|block}/<resource>?compute=<operation>
```

## 2. 适用场景

- 应用希望用同一套 HTTP/REST 接口适配文件、对象、块三种存储;
- 需要为上层 SDK 或 CLI 提供稳定的计算入口;
- 不希望把计算逻辑绑定到具体前端(S3/FUSE)内部。

## 3. 前置条件

见 [README.md](README.md) 通用前置条件。

## 4. 三种寻址方式

| 模式 | URL | 目标解析 |
| --- | --- | --- |
| file | `/api/compute/file/<filer-dir>/<filename>?compute=<operation>` | 直接使用 filer 路径 |
| object | `/api/compute/object/<bucket-name>/<object-key>?compute=<operation>` | 映射到 buckets 目录 |
| block | `/api/compute/block/<block-volume-name>?compute=<operation>` | 默认映射 `/blocks/<block-volume-name>` |
| block(显式) | `/api/compute/block/<volume-alias>?compute=<operation>&path=<backing-path>` | 按指定 backing 路径 |

## 5. 数据的增删改查入口

统一计算 API 只负责计算请求,不对数据本身提供普通 CRUD。
不同存储模式的数据 CRUD 分别按对应 SOP 执行:

| 模式 | CRUD 数据面 | 对应文档 |
| --- | --- | --- |
| file | filer HTTP / FUSE | [SOP-01-FS.md](SOP-01-FS.md) |
| object | S3 / filer buckets | [SOP-02-S3-Object.md](SOP-02-S3-Object.md) |
| block | `/blocks/` 块镜像文件 | [SOP-03-Block.md](SOP-03-Block.md) |

调用计算前,先确认目标数据已按对应接口完成增/改/查操作并可访问。

## 6. 计算调用步骤

1. 选择寻址模式:
   - 文件模式使用 `<filer-dir>/<filename>`;
   - 对象模式使用 `<bucket-name>/<object-key>`;
   - 块模式使用 `<block-volume-name>`;
2. 携带算子参数并执行:

   ```bash
   FILER=<filer-url>
   curl -s "$FILER/api/compute/file/<filer-dir>/<filename>?compute=<operation>"
   curl -s "$FILER/api/compute/object/<bucket-name>/<object-key>?compute=<operation>"
   curl -s "$FILER/api/compute/block/<block-volume-name>?compute=<operation>"
   ```

3. 比较响应与期望结果;
4. 检查 filer 日志确认跨 chunk 执行。

## 7. 响应格式

- 成功:HTTP 200,`Content-Type: text/plain`,body 为算子结果;
- 参数错误:HTTP 400;
- 目标不存在:HTTP 404;
- 服务端/volume 错误:HTTP 500;
- 方法不受支持:HTTP 405。

## 8. 错误处理

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| 400 目标为空 | 未提供资源 | 补全路径/bucket/volume |
| 400 对象格式错误 | object 缺少 bucket/key | 按 `object/<bucket>/<key>` 调用 |
| 400 算子不可合并 | 文本/顺序算子跨 chunk | 使用可合并算子或单 chunk 数据 |
| 404 | 目标不存在 | 检查上传 |
| 500 | volume 计算失败 | 查看日志 |

## 9. 上层接入建议

- 面向应用层可封装一个客户端,内部把文件/对象/块路径规范化为本 API 的 URL;
- 后续新增算子或聚合语义时,只需更新 filer/volume 侧,三种寻址入口自动同步生效。
