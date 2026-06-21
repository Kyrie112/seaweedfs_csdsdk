# SeaweedFS 可计算存储融合设计

## 目标

在 SeaweedFS 中支持“文件名 + 计算操作名”的请求形式：

```bash
curl "http://<filer-ip>:<filer-port>/dataset/a.csv?compute=sum"
```

用户只需要知道 Filer 上的文件路径和操作名，不需要知道底层 `fid`。Filer 负责把文件路径解析成 Volume 层可识别的 fileId，VolumeServer 负责读取 needle 数据、调用对应脚本，并把脚本输出返回给用户。

## 整体架构

```text
用户
  GET /path/file?compute=<op>
    |
    v
Filer
  文件路径 -> entry -> chunk fileId
    |
    v
VolumeServer
  fileId -> needle -> n.Data
    |
    v
Compute Script
  stdin(fd 0) = opened temp file for needle data
  argv[1] / env = temp file path
  stdout = compute result
    |
    v
用户收到计算结果
```

核心原则：

- 用户请求 Filer，而不是直接请求 Volume。
- Filer 负责“文件名/路径 -> fileId”的解析。
- Volume 负责“fileId -> needle 数据 -> 执行脚本”。
- 不修改 SeaweedFS 的存储格式、needle 索引、写入路径和复制流程。

## 用户请求方式

用户通过 Filer 发起请求：

```bash
curl "http://192.168.1.10:8888/dataset/a.csv?compute=sum"
```

其中：

- `192.168.1.10:8888` 是 Filer 的 IP 和端口。
- `/dataset/a.csv` 是 Filer 上的文件路径。
- `compute=sum` 表示计算操作名为 `sum`。

不建议用户直接请求 Volume：

```bash
curl "http://<volume-ip>:<volume-port>/3,01637037d6/a.csv?compute=sum"
```

除非用户已经知道底层 `fid`。Volume 层不负责根据文件名查找数据。

## 启动配置

VolumeServer 新增以下配置项：

```text
-volume.compute.enabled=false
-volume.compute.dir=""
-volume.compute.timeout=30s
-volume.compute.maxOutputMB=64
```

示例：

```bash
weed volume \
  -dir=/data/volume \
  -master=localhost:9333 \
  -volume.compute.enabled=true \
  -volume.compute.dir=/opt/seaweedfs/compute \
  -volume.compute.timeout=30s \
  -volume.compute.maxOutputMB=64
```

说明：

- `volume.compute.enabled`：是否启用 Volume 侧计算能力，默认关闭。
- `volume.compute.dir`：脚本目录。
- `volume.compute.timeout`：单次脚本执行超时。
- `volume.compute.maxOutputMB`：脚本 stdout 最大返回大小。

## 脚本映射规则

请求：

```text
?compute=sum
```

VolumeServer 会在脚本目录下查找：

```text
<volume.compute.dir>/sum
<volume.compute.dir>/sum.sh
```

例如：

```text
/opt/seaweedfs/compute/sum
/opt/seaweedfs/compute/sum.sh
```

脚本执行时：

- VolumeServer 会先把 needle 数据写入一个临时文件。
- `stdin` 是已经打开的临时文件句柄，也就是脚本里的 fd 0。
- 脚本第一个参数 `$1` 是这个临时文件路径。
- 环境变量 `SEAWEED_COMPUTE_INPUT_FILE` 也是这个临时文件路径。
- 环境变量 `SEAWEED_COMPUTE_INPUT_FD=0` 表示可从 fd 0 读取。
- `stdout` 会作为 HTTP 响应返回给用户。
- `stderr` 会在脚本失败时作为错误信息的一部分返回或记录。

示例脚本 `sum.sh`：

```sh
#!/bin/sh
awk '{s+=$1} END {print s}' "$SEAWEED_COMPUTE_INPUT_FILE"
```

## 脚本环境变量

VolumeServer 执行脚本时会注入以下环境变量：

```text
SEAWEED_COMPUTE_OPERATION
SEAWEED_FILE_NAME
SEAWEED_COMPUTE_INPUT_FILE
SEAWEED_COMPUTE_INPUT_FD
SEAWEED_VOLUME_ID
SEAWEED_NEEDLE_ID
SEAWEED_NEEDLE_NAME
SEAWEED_NEEDLE_MIME
SEAWEED_NEEDLE_SIZE
```

示例：

```sh
#!/bin/sh
echo "operation=$SEAWEED_COMPUTE_OPERATION"
echo "file=$SEAWEED_FILE_NAME"
echo "input=$SEAWEED_COMPUTE_INPUT_FILE"
wc -l "$SEAWEED_COMPUTE_INPUT_FILE"
```

## 执行顺序

### 1. 用户请求 Filer

```bash
curl "http://<filer-ip>:<filer-port>/dataset/a.csv?compute=sum"
```

### 2. Filer 查找文件 entry

Filer 根据 `/dataset/a.csv` 查找 metadata entry。

如果请求里没有 `compute` 参数，继续走原来的普通文件读取逻辑。

如果请求里存在：

```text
compute=sum
```

则进入 compute 代理逻辑。

### 3. Filer 将 entry 转换成 fileId

Filer 从 entry 中读取 chunk 信息，并解析 chunk manifest。

当前 MVP 只支持单 chunk 文件：

```text
entry -> one data chunk -> fileId
```

例如：

```text
/dataset/a.csv -> 3,01637037d6
```

以下情况当前会返回 400：

- inline content 文件。
- 没有 chunk 的文件。
- 多 chunk 文件。
- chunk 不是从 offset 0 覆盖完整文件。
- 加密 chunk。

### 4. Filer 查找 VolumeServer

Filer 通过 Master lookup 根据 fileId 找到 VolumeServer URL：

```text
3,01637037d6 -> http://volume-server:8080/3,01637037d6
```

然后构造 Volume 请求：

```text
http://volume-server:8080/3,01637037d6/a.csv?compute=sum
```

其中：

- `3,01637037d6` 用于 Volume 定位 needle。
- `a.csv` 用于传递文件名。
- `compute=sum` 用于告诉 Volume 执行哪个操作。

### 5. Filer 代理请求到 Volume

Filer 会把请求代理给 VolumeServer。

如果配置了 Volume read JWT，Filer 会生成 read token，并设置：

```text
Authorization: Bearer <jwt>
```

### 6. Volume 解析 vid/fid

VolumeServer 收到：

```text
/3,01637037d6/a.csv?compute=sum
```

解析出：

```text
vid      = 3
fid      = 01637037d6
filename = a.csv
compute  = sum
```

### 7. Volume 读取完整 needle 数据

普通读请求可能会走 streaming 优化。

但 compute 请求必须把完整数据交给脚本，因此带 `compute` 参数时会强制完整读取 needle 数据到：

```text
n.Data
```

### 8. Volume 执行 compute 分支

Volume 读取 needle 成功并完成 cookie 校验后，进入 compute 分支。

处理步骤：

1. 检查 `volume.compute.enabled` 是否开启。
2. 拒绝 `HEAD` compute 请求。
3. 拒绝 chunked manifest needle。
4. 校验操作名。
5. 如果 needle 数据是压缩的，先解压。
6. 根据操作名查找脚本。
7. 执行脚本。
8. 返回脚本 stdout。

### 9. Volume 返回脚本输出

脚本成功时：

```text
HTTP 200
Content-Type: application/octet-stream
Body: <script stdout>
```

Filer 会把 Volume 的响应透传给用户。

## 操作名安全限制

操作名只允许以下字符：

```text
a-z A-Z 0-9 _ - .
```

并禁止包含：

```text
..
```

非法示例：

```text
../sum
/bin/sh
a\b
```

脚本执行使用 `exec.CommandContext(scriptPath)`，不通过 `sh -c` 拼接用户输入，避免命令注入。

## 当前 MVP 限制

当前实现优先保证链路清晰和数据语义正确，因此限制如下：

- 只支持 Filer 路径请求，不支持 Volume 按文件名查找。
- 只支持单 chunk 文件。
- 不支持 inline content。
- 不支持多 chunk 文件。
- 不支持加密 chunk。
- 不支持 chunked manifest needle 的 Volume 侧直接计算。
- 脚本输出统一按 `application/octet-stream` 返回。

## 后续扩展方向

### 支持多 chunk 文件

多 chunk 文件需要明确计算语义：

1. Filer 聚合完整文件后发给某个 Volume 或本地执行。
2. 每个 chunk 在对应 Volume 上 map 计算，然后由 Filer reduce。
3. 新增脚本协议，区分 `map` 和 `reduce`。

推荐后续方向是：

```text
Volume: chunk-local map
Filer: aggregate/reduce
```

### 支持脚本返回 Content-Type

可以约定脚本通过 side-channel 返回 metadata，例如：

```text
stdout: result body
env/header file: content-type
```

或者支持脚本输出协议：

```text
Content-Type: application/json

{"result":123}
```

### 支持 Header 触发

除了 query 参数：

```text
?compute=sum
```

后续可以支持：

```text
Seaweed-Compute-Operation: sum
```

### 支持 allowlist

当前通过脚本目录和操作名校验限制脚本范围。

后续可以增加显式 allowlist：

```text
sum=/opt/seaweedfs/compute/sum.sh
count=/opt/seaweedfs/compute/count.sh
```

这样可以进一步收紧可执行脚本集合。

## 修改文件

- `weed/command/volume.go`
  - 新增 Volume compute 启动参数。
  - 将配置传给 `NewVolumeServer`。

- `weed/server/volume_server.go`
  - `VolumeServer` 新增 `computeConfig`。
  - `NewVolumeServer` 接收 `VolumeComputeConfig`。

- `weed/server/volume_compute.go`
  - 新增 Volume 侧 compute 执行逻辑。
  - 校验操作名。
  - 查找脚本。
  - 执行脚本。
  - 限制 timeout 和 stdout 大小。

- `weed/server/volume_server_handlers_read.go`
  - compute 请求强制完整读取 needle 数据。
  - needle 读取成功后进入 compute 分支。

- `weed/server/filer_server_handlers_read.go`
  - Filer 读请求识别 `?compute=<op>`。
  - compute 请求转入代理逻辑。

- `weed/server/filer_server_handlers_proxy.go`
  - 新增 Filer 到 Volume 的 compute 代理逻辑。
  - 将 entry 解析成单 chunk fileId。
  - lookup VolumeServer。
  - 带 read JWT 代理请求。

## 验证状态

已执行：

```bash
git diff --check
```

结果通过。

当前机器没有 Go 工具链，以下命令尚未执行：

```bash
gofmt
go test ./weed/server
```
