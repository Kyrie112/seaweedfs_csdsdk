# CSD 读再计算与计算下沉 CosBench 对比方案

本文档说明如何构造一个能显著体现 CSD 计算下沉收益的测试场景，并说明
CosBench 在这个测试中的使用方式。

要对比的两种方案是：

```text
方案 A：原本读再计算
  客户端从 SeaweedFS 读取完整对象，再在客户端执行 sum/count/filter。

方案 B：读取计算下沉
  客户端请求 Filer 的 ?compute=<op>，Volume/CSD 侧读取数据并执行计算，
  客户端只接收计算结果。
```

核心原则：

```text
测试对象要大，计算结果要小。
```

例如对 1 GiB 文本数字文件求和，方案 A 需要把 1 GiB 数据传到客户端，
方案 B 只返回几十字节结果。这样网络传输、客户端 CPU、主机侧数据搬运差异
会非常明显。

## 1. CosBench 在该测试中的角色

标准 CosBench 的 S3 driver 可以很好地完成：

```text
批量创建 bucket
批量写入对象
批量读取完整对象
统计 S3 PUT/GET 的吞吐和延迟
```

但标准 CosBench 不会把 GET 到的数据交给 `awk`、`wc` 或你的自定义程序继续
计算。因此，CosBench 不能单独完成完整的“读再计算”端到端测试。

推荐分工：

```text
CosBench:
  1. 构造大对象数据集
  2. 测完整对象 GET 的基线吞吐和延迟

客户端脚本:
  1. 测“下载完整对象 + 客户端计算”的真实端到端耗时

Filer compute 请求:
  1. 测“读取计算下沉”的端到端耗时
```

如果你手里的 CosBench 版本支持自定义 HTTP GET URL，也可以让 CosBench 直接
请求：

```text
http://192.168.0.9:8888/dataset/object-000001.txt?compute=sum
```

但在普通 CosBench S3 workload 下，compute 请求建议用脚本或 `wrk` 来测。

## 2. 推荐测试场景

### 2.1 主场景：大文本数字文件 sum

对象内容：

```text
1
2
3
...
```

操作：

```text
sum：对每行数字求和，只返回一个数字
```

对比：

```text
方案 A：curl GET 完整文件 | awk 求和
方案 B：curl GET 文件?compute=sum
```

推荐文件大小：

```text
64 MiB
256 MiB
512 MiB
```

如果要测试 1 GiB 或更大文件，需要确保 Filer 上传时不会把文件拆成多个
chunk。当前 compute MVP 更适合单 chunk 文件。

启动 Filer 时建议设置：

```bash
-maxMB=1024
```

这样 512 MiB 文件可以保持单 chunk。

### 2.2 辅助场景：日志关键字计数

对象内容：

```text
timestamp level user_id message
```

操作：

```text
统计 ERROR 行数
统计指定 user_id 出现次数
```

这个场景也很适合计算下沉，因为客户端最终只需要一个计数。

## 3. SeaweedFS 准备

三台 volume 都启用 compute。为了减少临时文件读写开销，建议分别测试
`tempfile` 和 `stdin` 两种模式。

临时文件模式：

```bash
-volume.compute.enabled=true \
-volume.compute.dir=/home/dess/compute_program \
-volume.compute.input=tempfile
```

stdin 模式：

```bash
-volume.compute.enabled=true \
-volume.compute.dir=/home/dess/compute_program \
-volume.compute.input=stdin
```

Filer 建议：

```bash
nohup ./weed filer \
  -master=192.168.0.9:9333,192.168.0.10:9333,192.168.0.11:9333 \
  -port=8888 \
  -ip=192.168.0.9 \
  -ip.bind=0.0.0.0 \
  -maxMB=1024 \
  > ./filer.log 2>&1 &
```

S3 Gateway 给 CosBench 使用：

```bash
nohup ./weed s3 \
  -filer=192.168.0.9:8888 \
  -port=8333 \
  -ip.bind=0.0.0.0 \
  -config=/home/dess/s3.json \
  > ./s3.log 2>&1 &
```

CosBench endpoint：

```text
http://192.168.0.9:8333
```

## 4. 计算脚本

### 4.1 sum.sh 兼容 tempfile 和 stdin

在三台 volume 节点创建：

```bash
cat > /home/dess/compute_program/sum.sh <<'EOF'
#!/bin/bash
if [ -n "$1" ] && [ -f "$1" ]; then
  awk '{s += $1} END {print s}' "$1"
else
  awk '{s += $1} END {print s}'
fi
EOF

chmod +x /home/dess/compute_program/sum.sh
```

这样同一个脚本可以同时支持：

```text
-volume.compute.input=tempfile
-volume.compute.input=stdin
```

### 4.2 linecount.sh

```bash
cat > /home/dess/compute_program/linecount.sh <<'EOF'
#!/bin/bash
if [ -n "$1" ] && [ -f "$1" ]; then
  wc -l < "$1"
else
  wc -l
fi
EOF

chmod +x /home/dess/compute_program/linecount.sh
```

## 5. 数据集构造

### 5.1 推荐：通过 Filer HTTP 构造可计算数据集

为了让 compute 路径最清晰，推荐直接写入 `/dataset`：

```bash
mkdir -p /tmp/csd-sum-data

for size in 64 256 512; do
  file=/tmp/csd-sum-data/numbers-${size}m.txt
  yes 1 | head -c ${size}M > "$file"
  curl -s -F file=@"$file" \
    "http://192.168.0.9:8888/dataset/numbers-${size}m.txt" >/dev/null
done
```

这些文件的 sum 结果应接近文件中的行数。因为每行是 `1`，所以 sum 和
linecount 都适合校验。

### 5.2 CosBench 构造 S3 GET baseline 数据集

CosBench 适合构造 S3 对象读写基线。下面的 XML 用于写入和读取 256 MiB
对象。

文件名：

```text
seaweedfs-csd-s3-256m.xml
```

内容：

```xml
<?xml version="1.0" encoding="UTF-8" ?>
<workload name="seaweedfs-csd-s3-256m">
  <storage type="s3" config="accesskey=admin;secretkey=secret;endpoint=http://192.168.0.9:8333;path_style_access=true" />

  <workflow>
    <workstage name="init">
      <work type="init" workers="1" config="cprefix=csd-bench;containers=r(1,1)" />
    </workstage>

    <workstage name="write-256m">
      <work type="write" workers="8" runtime="300"
            config="cprefix=csd-bench;containers=r(1,1);objects=r(1,1000);sizes=c(256)MB" />
    </workstage>

    <workstage name="read-256m">
      <work type="read" workers="8" runtime="300"
            config="cprefix=csd-bench;containers=r(1,1);objects=r(1,1000)" />
    </workstage>
  </workflow>
</workload>
```

提交到 CosBench：

```bash
curl -F submit=@seaweedfs-csd-s3-256m.xml \
  http://<cosbench-controller>:19088/controller/submit
```

CosBench 结果用于说明：

```text
SeaweedFS/S3 完整对象读的吞吐上限是多少
读再计算方案至少需要承受这个完整对象读取成本
```

## 6. 方案 A：原本读再计算

### 6.1 单请求端到端测试

客户端下载完整文件，再本地求和：

```bash
/usr/bin/time -f "elapsed=%e user=%U sys=%S maxrss=%M" \
  bash -c 'curl -s "http://192.168.0.9:8888/dataset/numbers-256m.txt" | awk "{s += \$1} END {print s}"'
```

记录下载字节数：

```bash
curl -s -w "time=%{time_total} size=%{size_download}\n" \
  "http://192.168.0.9:8888/dataset/numbers-256m.txt" \
  -o /tmp/numbers-256m.txt

/usr/bin/time -f "compute_elapsed=%e user=%U sys=%S" \
  awk '{s += $1} END {print s}' /tmp/numbers-256m.txt
```

这两个命令可以拆分出：

```text
完整读取耗时
客户端本地计算耗时
客户端接收的数据量
```

### 6.2 并发测试

用 `wrk` 测完整读取压力：

```bash
cat > /tmp/read-compute-baseline-urls.txt <<'EOF'
/dataset/numbers-64m.txt
/dataset/numbers-256m.txt
/dataset/numbers-512m.txt
EOF
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

执行：

```bash
wrk -t8 -c16 -d300s -s /tmp/random-url.lua \
  http://192.168.0.9:8888 -- /tmp/read-compute-baseline-urls.txt
```

说明：这一步只测“读再计算”里的完整读取成本。客户端真实计算成本用 6.1
的 `curl | awk` 单请求或自写并发脚本补充。

## 7. 方案 B：读取计算下沉

### 7.1 单请求端到端测试

```bash
curl -s -w "time=%{time_total} size=%{size_download}\n" \
  "http://192.168.0.9:8888/dataset/numbers-256m.txt?compute=sum" \
  -o /tmp/sum-256m.out

cat /tmp/sum-256m.out
```

预期：

```text
size_download 只有几十字节
time_total 是下沉计算端到端耗时
```

### 7.2 并发测试

```bash
cat > /tmp/compute-sum-urls.txt <<'EOF'
/dataset/numbers-64m.txt?compute=sum
/dataset/numbers-256m.txt?compute=sum
/dataset/numbers-512m.txt?compute=sum
EOF

wrk -t8 -c16 -d300s -s /tmp/random-url.lua \
  http://192.168.0.9:8888 -- /tmp/compute-sum-urls.txt
```

分别在以下两种 volume 模式下运行：

```text
-volume.compute.input=tempfile
-volume.compute.input=stdin
```

## 8. 指标采集

三台 SeaweedFS 节点都执行：

```bash
mkdir -p /home/dess/bench-logs

iostat -dxm 1 > /home/dess/bench-logs/iostat-$(hostname)-$(date +%Y%m%d-%H%M%S).log &
pid_iostat=$!

sar -n DEV 1 > /home/dess/bench-logs/sar-net-$(hostname)-$(date +%Y%m%d-%H%M%S).log &
pid_sar=$!

pidstat -urd -p ALL 1 > /home/dess/bench-logs/pidstat-$(hostname)-$(date +%Y%m%d-%H%M%S).log &
pid_pidstat=$!
```

停止：

```bash
kill $pid_iostat $pid_sar $pid_pidstat
```

每轮保存：

```bash
curl http://192.168.0.9:9333/cluster/status | tee cluster-status.json
curl http://192.168.0.9:9333/dir/status | tee dir-status.json
df -h /home/dess/data | tee disk-space.txt
lsblk | tee lsblk.txt
```

重点看：

```text
客户端接收网络流量：方案 B 应显著低于方案 A
Filer 节点出网流量：方案 B 应显著低于方案 A
Volume/CSD 读吞吐：两种方案都需要读取原始数据
Volume 临时写入：tempfile 模式应高于 stdin 模式
客户端 CPU：读再计算更高
```

## 9. 推荐执行顺序

1. 确保三台 master、三台 volume、一个 filer 正常。
2. 启动 S3 gateway。
3. 用 CosBench 跑 S3 PUT/GET baseline，确认完整对象读写吞吐。
4. 用 Filer HTTP 写入 `/dataset/numbers-64m.txt`、`256m`、`512m`。
5. 在 `tempfile` 模式下跑方案 A 单请求和并发完整读取。
6. 在 `tempfile` 模式下跑方案 B compute=sum。
7. 重启三台 volume，切到 `-volume.compute.input=stdin`。
8. 重复方案 B compute=sum。
9. 汇总 CosBench、curl/wrk、iostat/sar/pidstat 的结果。

## 10. 结果记录表

| 文件大小 | 并发 | 场景 | 工具 | 总耗时 | Ops/s | 客户端下载量 | Filer 出网 | Volume 读 | Volume 写 | 客户端 CPU |
| --- | ---: | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 256 MiB | 1 | 读再计算 | curl + awk | | | 256 MiB | 高 | 高 | 低 | 高 |
| 256 MiB | 1 | 下沉 tempfile | curl compute | | | 几十字节 | 低 | 高 | 高 | 低 |
| 256 MiB | 1 | 下沉 stdin | curl compute | | | 几十字节 | 低 | 高 | 低 | 低 |
| 256 MiB | 16 | 完整读 baseline | wrk | | | 高 | 高 | 高 | 低 | 中 |
| 256 MiB | 16 | 下沉 stdin | wrk compute | | | 低 | 低 | 高 | 低 | 低 |

关键计算：

```text
网络节省比例 = 读再计算客户端下载量 / 下沉计算客户端下载量
端到端加速比 = 读再计算耗时 / 下沉计算耗时
stdin 优化比例 = tempfile 耗时 / stdin 耗时
```

## 11. 预期结论

如果测试构造正确，应该看到：

```text
读再计算：客户端和 Filer 网络流量随文件大小线性增长
计算下沉：客户端接收数据量接近常数，只与结果大小有关
tempfile：Volume 节点有额外临时文件写入
stdin：Volume 节点临时写入下降
```

如果下沉计算没有明显更快，优先检查：

```text
文件是否被拆成多 chunk
compute 脚本是否每次 fork/exec 成为瓶颈
Filer 是否成为单点瓶颈
Volume 是否仍在写临时文件
CSD 设备读吞吐是否已经饱和
```

