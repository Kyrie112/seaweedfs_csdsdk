# CSD CosBench 性能对比测试方案

本文档用于规划 SeaweedFS 可计算存储 CSD 的性能对比测试。目标是先用
CosBench 构造稳定、可重复的数据场景，再分别测试普通读、临时文件计算、
stdin 计算三类路径的性能。

当前三节点部署假设如下：

```text
192.168.0.9   master + filer + volume
192.168.0.10  master + volume
192.168.0.11  master + volume
```

其中 Filer 入口为：

```text
http://192.168.0.9:8888
```

CSD compute 请求形式为：

```text
http://192.168.0.9:8888/<文件路径>?compute=<计算操作名>
```

例如：

```bash
curl "http://192.168.0.9:8888/dataset/test.txt?compute=uppercase"
```

## 1. 测试目标

需要对比以下几类场景：

```text
S3 baseline       CosBench 通过 S3 API 测普通对象 PUT/GET
Filer baseline    Filer HTTP 普通读，不带 compute
compute-tempfile  Volume compute 使用临时文件输入
compute-stdin     Volume compute 使用 stdin 输入，减少临时文件读写
```

重点观察：

```text
吞吐量       MiB/s、ops/s
延迟         avg、p50、p95、p99
CPU          filer、volume、计算脚本进程
磁盘 IO      read/write MiB/s、IOPS、await、util
网络 IO      每台节点 RX/TX
CSD 指标     如果设备有 vendor 工具，也记录设备侧吞吐和利用率
```

预期判断：

```text
compute-stdin 相比 compute-tempfile 应减少临时文件写盘和再读开销。
Filer baseline 是不做计算时的读路径上限。
S3 baseline 用于证明 SeaweedFS 对象存储路径本身的吞吐能力。
```

## 2. 为什么 CosBench 只负责一部分测试

CosBench 很适合构造 S3 对象场景：

```text
创建 bucket
批量 PUT 对象
批量 GET 对象
控制对象大小、对象数量、并发度
输出吞吐和延迟报告
```

但当前 CSD compute 接口不是 S3 API，而是 Filer HTTP 读路径上的 query 参数：

```text
GET /dataset/object-000001.txt?compute=uppercase
```

所以推荐方案是：

```text
CosBench: 构造数据集 + 测 S3 PUT/GET baseline
HTTP 压测工具: 测 Filer baseline 和 compute 请求
```

如果你手里的 CosBench 版本支持自定义 HTTP GET URL 和 query 参数，也可以用
CosBench 直接压测 compute URL。否则，用 `wrk` 或 `hey` 测 compute 更直接。

## 3. 测试矩阵

每个组合至少跑 3 次，最终取中位数。

| 场景 | 工具 | 入口 | 目的 |
| --- | --- | --- | --- |
| S3 write prepare | CosBench | `192.168.0.9:8333` | 构造对象数据集 |
| S3 read baseline | CosBench | `192.168.0.9:8333` | 测 S3 普通读性能 |
| Filer read baseline | wrk/hey | `192.168.0.9:8888` | 测 Filer 普通读性能 |
| compute-tempfile | wrk/hey | `192.168.0.9:8888?compute=uppercase` | 测旧计算路径 |
| compute-stdin | wrk/hey | `192.168.0.9:8888?compute=uppercase` | 测减少临时文件开销后的路径 |

建议对象大小：

```text
4 KiB       小对象开销
64 KiB      小文件读路径
1 MiB       常见对象大小
16 MiB      吞吐测试
64 MiB      大对象吞吐测试
```

建议并发度：

```text
1, 4, 16, 64, 128
```

每组测试建议：

```text
warmup:    60 秒
duration:  300 秒
cooldown:  60 秒
```

## 4. SeaweedFS 启动方式

### 4.1 compute-tempfile 模式

三台 volume 都使用：

```bash
-volume.compute.enabled=true \
-volume.compute.dir=/home/dess/compute_program \
-volume.compute.input=tempfile
```

该模式会把 needle 数据写入临时文件，再把临时文件路径传给脚本 `$1`。

### 4.2 compute-stdin 模式

三台 volume 都使用：

```bash
-volume.compute.enabled=true \
-volume.compute.dir=/home/dess/compute_program \
-volume.compute.input=stdin
```

该模式不会创建临时文件，而是把已经读出的 needle 数据直接接到脚本的
stdin。

`uppercase.sh` 应改成：

```bash
#!/bin/bash
tr '[:lower:]' '[:upper:]'
```

并在三台 volume 节点执行：

```bash
chmod +x /home/dess/compute_program/uppercase.sh
```

## 5. 启动 S3 Gateway 给 CosBench 使用

在 `192.168.0.9` 上启动 S3：

```bash
cd /home/dess

nohup ./weed s3 \
  -filer=192.168.0.9:8888 \
  -port=8333 \
  -ip.bind=0.0.0.0 \
  > ./s3.log 2>&1 &
```

CosBench endpoint 使用：

```text
http://192.168.0.9:8333
```

如果你的 SeaweedFS S3 配置了账号密码，就在 CosBench XML 里写相同的
`accesskey` 和 `secretkey`。如果是开发环境无鉴权，也可以先用固定值：

```text
accesskey=admin
secretkey=secret
```

## 6. CosBench 构造 S3 场景

创建 CosBench workload 文件：

```text
seaweedfs-csd-s3-1m.xml
```

示例内容：

```xml
<?xml version="1.0" encoding="UTF-8" ?>
<workload name="seaweedfs-csd-s3-1m">
  <storage type="s3" config="accesskey=admin;secretkey=secret;endpoint=http://192.168.0.9:8333;path_style_access=true" />

  <workflow>
    <workstage name="init">
      <work type="init" workers="1" config="cprefix=csd-bench;containers=r(1,1)" />
    </workstage>

    <workstage name="write-1m">
      <work type="write" workers="32" runtime="300"
            config="cprefix=csd-bench;containers=r(1,1);objects=r(1,100000);sizes=c(1)MB" />
    </workstage>

    <workstage name="read-1m">
      <work type="read" workers="32" runtime="300"
            config="cprefix=csd-bench;containers=r(1,1);objects=r(1,100000)" />
    </workstage>
  </workflow>
</workload>
```

说明：

```text
workers=32            并发工作线程
objects=r(1,100000)   对象编号范围
sizes=c(1)MB          固定 1 MiB 对象
runtime=300           持续 300 秒
```

提交 workload：

```bash
curl -F submit=@seaweedfs-csd-s3-1m.xml http://<cosbench-controller>:19088/controller/submit
```

也可以在 CosBench Web UI 里上传 XML。

不同对象大小建议分别建 XML：

```text
seaweedfs-csd-s3-4k.xml
seaweedfs-csd-s3-64k.xml
seaweedfs-csd-s3-1m.xml
seaweedfs-csd-s3-16m.xml
seaweedfs-csd-s3-64m.xml
```

## 7. Compute 数据集构造方式

为了让 compute 测试路径简单，推荐单独通过 Filer HTTP 构造 `/dataset`
目录数据，而不是强依赖 S3 bucket 内部路径。

在压测客户端执行：

```bash
mkdir -p /tmp/csd-bench-data

for i in $(seq -w 1 100000); do
  dd if=/dev/urandom bs=1M count=1 status=none | base64 | head -c 1048576 > /tmp/csd-bench-data/object-$i.txt
  curl -s -F file=@/tmp/csd-bench-data/object-$i.txt \
    "http://192.168.0.9:8888/dataset/object-$i.txt" >/dev/null
done
```

如果你希望完全由 CosBench 构造数据，也可以尝试通过 Filer bucket 路径访问
S3 对象：

```text
/buckets/csd-bench/object-000001
```

但为了减少变量，第一轮建议使用 `/dataset/object-xxxxx.txt`。

## 8. 用 wrk 测 Filer 和 compute

安装工具：

```bash
sudo apt-get update
sudo apt-get install -y wrk curl
```

生成 URL 列表：

```bash
seq -w 1 100000 | awk '{print "/dataset/object-" $1 ".txt"}' \
  > /tmp/filer-read-urls.txt

seq -w 1 100000 | awk '{print "/dataset/object-" $1 ".txt?compute=uppercase"}' \
  > /tmp/filer-compute-uppercase-urls.txt
```

创建 `/tmp/random-url.lua`：

```lua
local urls = {}
local counter = 0

function init(args)
  local path = args[1]
  for line in io.lines(path) do
    urls[#urls + 1] = line
  end
end

request = function()
  counter = counter + 1
  local url = urls[(counter % #urls) + 1]
  return wrk.format("GET", url)
end
```

Filer 普通读 baseline：

```bash
wrk -t8 -c64 -d300s -s /tmp/random-url.lua http://192.168.0.9:8888 -- /tmp/filer-read-urls.txt
```

compute 测试：

```bash
wrk -t8 -c64 -d300s -s /tmp/random-url.lua http://192.168.0.9:8888 -- /tmp/filer-compute-uppercase-urls.txt
```

批量跑不同并发：

```bash
for c in 1 4 16 64 128; do
  wrk -t8 -c$c -d300s -s /tmp/random-url.lua http://192.168.0.9:8888 -- /tmp/filer-compute-uppercase-urls.txt \
    | tee "compute-c${c}.log"
done
```

流程上要先在 `compute-tempfile` 模式跑一遍，再重启三台 volume 到
`compute-stdin` 模式后跑同样命令。

## 9. 系统指标采集

每次压测开始前，在三台 SeaweedFS 节点都启动采集：

```bash
mkdir -p /home/dess/bench-logs

iostat -dxm 1 > /home/dess/bench-logs/iostat-$(hostname)-$(date +%Y%m%d-%H%M%S).log &
pid_iostat=$!

sar -n DEV 1 > /home/dess/bench-logs/sar-net-$(hostname)-$(date +%Y%m%d-%H%M%S).log &
pid_sar=$!

pidstat -urd -p ALL 1 > /home/dess/bench-logs/pidstat-$(hostname)-$(date +%Y%m%d-%H%M%S).log &
pid_pidstat=$!
```

测试结束后停止：

```bash
kill $pid_iostat $pid_sar $pid_pidstat
```

每轮测试至少保存：

```bash
curl http://192.168.0.9:9333/cluster/status | tee cluster-status.json
curl http://192.168.0.9:9333/dir/status | tee dir-status.json
df -h /home/dess/data | tee disk-space.txt
lsblk | tee lsblk.txt
```

如果 CSD 设备有厂商监控工具，也要同步记录设备侧读写吞吐和计算单元利用率。

## 10. 缓存控制

每个场景建议分两种状态：

```text
cold-ish  重启服务或制造缓存压力后测试
warm      紧接着重复测试
```

如果测试机器允许，可以在三台 volume 节点测试前执行：

```bash
sync
echo 3 | sudo tee /proc/sys/vm/drop_caches
```

不要在共享生产机器上执行该命令。

## 11. 推荐执行顺序

1. 三台机器部署同一个 `weed` 二进制。
2. 启动 3 个 master。
3. 启动 3 个 volume，先使用 `-volume.compute.input=tempfile`。
4. 在 `192.168.0.9` 启动 filer 和 S3 gateway。
5. 用 CosBench 执行 S3 write，构造对象数据。
6. 用 CosBench 执行 S3 read baseline。
7. 用 Filer HTTP 构造 `/dataset` compute 数据集。
8. 用 wrk 测 Filer 普通读 baseline。
9. 用 wrk 测 `compute-tempfile`。
10. 重启 3 个 volume，改为 `-volume.compute.input=stdin`。
11. 用 wrk 测 `compute-stdin`。
12. 汇总三次运行的中位数。

## 12. 结果记录表

| 对象大小 | 并发 | 场景 | Ops/s | MiB/s | Avg | P95 | P99 | Volume CPU | CSD 读 MiB/s | 额外写 MiB/s |
| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 MiB | 64 | S3 read baseline | | | | | | | | |
| 1 MiB | 64 | Filer read baseline | | | | | | | | |
| 1 MiB | 64 | compute-tempfile | | | | | | | | |
| 1 MiB | 64 | compute-stdin | | | | | | | | |

重点计算：

```text
stdin 提升比例 = compute-stdin throughput / compute-tempfile throughput
临时文件写放大 = compute-tempfile 时额外观察到的磁盘写 MiB/s
compute 开销 = Filer baseline latency - compute latency 的差异分析
```

## 13. 结果解读

如果 `compute-stdin` 吞吐更高、延迟更低，并且 volume 节点磁盘写入明显下降，
说明临时文件路径确实是额外开销来源。

如果 `compute-stdin` 和 `compute-tempfile` 接近，瓶颈可能在：

```text
needle 从 CSD 读出的开销
Filer 代理开销
每次请求 fork/exec 脚本的进程开销
uppercase 脚本 CPU 开销
网络吞吐
单 Filer 入口瓶颈
```

如果 compute 两种模式都明显慢于 Filer baseline，下一步优化方向应该是：

```text
减少每次请求 fork/exec
改成长驻 compute worker
进一步做 direct compute：向 CSD 程序传 volume dat 文件路径、offset、size
```

