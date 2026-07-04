# Linux 上使用 CosBench 测试 SeaweedFS/CSD 的完整流程

本文档给出从零开始在 Linux 上安装、启动、使用 CosBench，并接入当前
SeaweedFS/CSD 集群进行性能测试的完整步骤。

当前集群假设：

```text
192.168.0.9   master + filer + volume + s3 gateway
192.168.0.10  master + volume
192.168.0.11  master + volume
```

Filer 入口：

```text
http://192.168.0.9:8888
```

S3 Gateway 入口：

```text
http://192.168.0.9:8333
```

CosBench 的作用：

```text
1. 构造大量 S3 对象数据
2. 测 SeaweedFS S3 PUT/GET baseline
3. 为“读完整对象再计算”提供完整读取成本参考
```

注意：标准 CosBench S3 driver 不会自动对 GET 到的数据执行 `awk`、`sum`
或其他客户端计算。因此，CSD 核心对比建议使用：

```text
CosBench      测 S3 完整对象读写 baseline
curl + awk    测“读完整文件到客户端再计算”
?compute=sum  测“读取计算下沉”
```

## 1. 准备 CosBench 客户端机器

建议把 CosBench 安装在独立压测客户端上，不要和 SeaweedFS volume 节点混跑。
如果暂时没有独立机器，也可以先在 `192.168.0.9` 上做功能验证。

安装基础依赖：

```bash
sudo apt-get update
sudo apt-get install -y unzip curl wget lsof sysstat
```

CosBench 较老，建议优先使用 Java 8：

```bash
sudo apt-get install -y openjdk-8-jdk
java -version
```

如果系统源没有 Java 8，可以先用当前系统 Java 试跑；如果启动失败，再安装
OpenJDK/Temurin 8。

## 2. 下载并解压 CosBench

官方仓库：

```text
https://github.com/intel-cloud/cosbench
```

Release 页面：

```text
https://github.com/intel-cloud/cosbench/releases
```

在 Linux 上执行：

```bash
cd /home/dess
mkdir -p cosbench
cd cosbench
```

尝试下载 v0.4.2：

```bash
wget -O cosbench.zip https://github.com/intel-cloud/cosbench/releases/download/v0.4.2/0.4.2.zip
```

如果该链接不可用，就打开 release 页面，复制实际 zip 下载链接后执行：

```bash
wget -O cosbench.zip '你复制的 release zip 下载链接'
```

解压：

```bash
unzip cosbench.zip
ls
```

进入解压目录。目录名可能是 `0.4.2`、`cosbench-0.4.2` 或类似名称：

```bash
cd 0.4.2
chmod +x *.sh
chmod +x cli/*.sh 2>/dev/null || true
```

## 3. 启动 CosBench

单机模式启动 controller 和 driver：

```bash
./start-all.sh
```

检查端口：

```bash
lsof -i:19088
lsof -i:18088
```

Web UI：

```text
http://<cosbench机器IP>:19088/controller
```

本机快速检查：

```bash
curl http://127.0.0.1:19088/controller/index.html
```

停止 CosBench：

```bash
./stop-all.sh
```

查看日志：

```bash
ls -lh log
tail -f log/system.log
```

如果启动失败，优先检查：

```bash
java -version
端口 19088/18088 是否被占用
当前目录是否有执行权限
```

## 4. 准备 SeaweedFS S3 Gateway

### 4.1 创建 S3 账号配置

在 `192.168.0.9` 上创建 `/home/dess/s3.json`：

```bash
cat > /home/dess/s3.json <<'EOF'
{
  "identities": [
    {
      "name": "cosbench",
      "credentials": [
        {
          "accessKey": "admin",
          "secretKey": "secret"
        }
      ],
      "actions": [
        "Admin",
        "Read",
        "Write"
      ]
    }
  ]
}
EOF
```

### 4.2 启动 S3 Gateway

在 `192.168.0.9` 上执行：

```bash
cd /home/dess

nohup ./weed s3 \
  -filer=192.168.0.9:8888 \
  -port=8333 \
  -ip.bind=0.0.0.0 \
  -config=/home/dess/s3.json \
  > ./s3.log 2>&1 &
```

检查：

```bash
ps aux | grep "weed s3" | grep -v grep
tail -f /home/dess/s3.log
curl http://192.168.0.9:8333/
```

CosBench 中使用：

```text
endpoint=http://192.168.0.9:8333
accesskey=admin
secretkey=secret
path_style_access=true
```

## 5. 编写 CosBench Workload

在 CosBench 机器上创建：

```bash
cat > seaweedfs-csd-s3-1m.xml <<'EOF'
<?xml version="1.0" encoding="UTF-8" ?>
<workload name="seaweedfs-csd-s3-1m">
  <storage type="s3" config="accesskey=admin;secretkey=secret;endpoint=http://192.168.0.9:8333;path_style_access=true" />

  <workflow>
    <workstage name="init">
      <work type="init" workers="1" config="cprefix=csd-bench;containers=r(1,1)" />
    </workstage>

    <workstage name="write-1m">
      <work type="write" workers="32" runtime="300"
            config="cprefix=csd-bench;containers=r(1,1);objects=r(1,10000);sizes=c(1)MB" />
    </workstage>

    <workstage name="read-1m">
      <work type="read" workers="32" runtime="300"
            config="cprefix=csd-bench;containers=r(1,1);objects=r(1,10000)" />
    </workstage>
  </workflow>
</workload>
EOF
```

参数含义：

```text
cprefix=csd-bench        bucket/container 前缀
containers=r(1,1)        只创建 1 个 bucket
objects=r(1,10000)       对象编号 1 到 10000
sizes=c(1)MB             每个对象固定 1 MiB
workers=32               并发 worker 数
runtime=300              每个阶段运行 300 秒
```

## 6. 提交 CosBench 任务

如果 CosBench controller 在本机：

```bash
curl -F submit=@seaweedfs-csd-s3-1m.xml \
  http://127.0.0.1:19088/controller/submit
```

如果 controller 在其他机器：

```bash
curl -F submit=@seaweedfs-csd-s3-1m.xml \
  http://<cosbench-controller-ip>:19088/controller/submit
```

也可以打开 Web UI 上传：

```text
http://<cosbench-controller-ip>:19088/controller
```

查看任务状态：

```bash
curl http://127.0.0.1:19088/controller/workloads
```

或者直接看 Web UI。

## 7. 构造不同对象大小的测试

建议至少准备这些 workload：

```text
4 KiB
64 KiB
1 MiB
16 MiB
64 MiB
256 MiB
```

只需要改 XML 中的：

```text
sizes=c(1)MB
```

例如 256 MiB：

```xml
<work type="write" workers="8" runtime="300"
      config="cprefix=csd-bench;containers=r(1,1);objects=r(1,1000);sizes=c(256)MB" />
```

大对象测试建议减少对象数量和 worker 数，避免一次性占用过多空间。

## 8. 采集 SeaweedFS 节点系统指标

每轮 CosBench 测试开始前，在三台 SeaweedFS 节点执行：

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

每轮保存集群状态：

```bash
curl http://192.168.0.9:9333/cluster/status | tee cluster-status.json
curl http://192.168.0.9:9333/dir/status | tee dir-status.json
df -h /home/dess/data | tee disk-space.txt
lsblk | tee lsblk.txt
```

## 9. 接入 CSD 读再计算与计算下沉对比

CosBench 完成的是 S3 完整对象 PUT/GET baseline。要体现 CSD 计算下沉，需要
额外构造 Filer 路径的数据并做两个对比。

### 9.1 构造可计算数据集

在压测客户端执行：

```bash
mkdir -p /tmp/csd-sum-data

for size in 64 256 512; do
  file=/tmp/csd-sum-data/numbers-${size}m.txt
  yes 1 | head -c ${size}M > "$file"
  curl -s -F file=@"$file" \
    "http://192.168.0.9:8888/dataset/numbers-${size}m.txt" >/dev/null
done
```

注意：当前 compute MVP 更适合单 chunk 文件。测试 512 MiB 文件时，Filer
建议用 `-maxMB=1024` 启动。

### 9.2 原本方案：读完整文件再客户端计算

```bash
/usr/bin/time -f "elapsed=%e user=%U sys=%S maxrss=%M" \
  bash -c 'curl -s "http://192.168.0.9:8888/dataset/numbers-256m.txt" | awk "{s += \$1} END {print s}"'
```

记录完整下载量：

```bash
curl -s -w "time=%{time_total} size=%{size_download}\n" \
  "http://192.168.0.9:8888/dataset/numbers-256m.txt" \
  -o /tmp/numbers-256m.txt
```

### 9.3 下沉方案：Filer compute

```bash
curl -s -w "time=%{time_total} size=%{size_download}\n" \
  "http://192.168.0.9:8888/dataset/numbers-256m.txt?compute=sum" \
  -o /tmp/sum-256m.out

cat /tmp/sum-256m.out
```

这里 `size_download` 应只有几十字节，而不是 256 MiB。

## 10. 并发 compute 测试

安装 `wrk`：

```bash
sudo apt-get install -y wrk
```

创建 URL 文件：

```bash
cat > /tmp/compute-sum-urls.txt <<'EOF'
/dataset/numbers-64m.txt?compute=sum
/dataset/numbers-256m.txt?compute=sum
/dataset/numbers-512m.txt?compute=sum
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
for c in 1 4 16 64; do
  wrk -t8 -c$c -d300s -s /tmp/random-url.lua \
    http://192.168.0.9:8888 -- /tmp/compute-sum-urls.txt \
    | tee "compute-sum-c${c}.log"
done
```

分别在两种 volume 模式下跑：

```text
-volume.compute.input=tempfile
-volume.compute.input=stdin
```

## 11. 推荐完整执行顺序

1. 在 CosBench 客户端安装并启动 CosBench。
2. 在 `192.168.0.9` 启动 SeaweedFS S3 Gateway。
3. 用 CosBench 执行 S3 write，构造对象数据。
4. 用 CosBench 执行 S3 read，得到完整对象 GET baseline。
5. 用 Filer HTTP 构造 `/dataset/numbers-64m.txt`、`256m`、`512m`。
6. 用 `curl | awk` 测读完整文件再客户端计算。
7. 用 `?compute=sum` 测计算下沉。
8. 切换 volume 的 `-volume.compute.input=tempfile/stdin`，重复 compute 测试。
9. 同步采集 `iostat`、`sar`、`pidstat`。
10. 汇总 CosBench 报告、curl 输出、wrk 输出和系统指标。

## 12. 结果记录表

| 文件大小 | 并发 | 场景 | 工具 | 总耗时 | Ops/s | 客户端下载量 | Filer 出网 | Volume 读 | Volume 写 |
| --- | ---: | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 256 MiB | 1 | S3 GET baseline | CosBench | | | 256 MiB/对象 | 高 | 高 | 低 |
| 256 MiB | 1 | 读再计算 | curl + awk | | | 256 MiB | 高 | 高 | 低 |
| 256 MiB | 1 | 下沉 tempfile | curl compute | | | 几十字节 | 低 | 高 | 高 |
| 256 MiB | 1 | 下沉 stdin | curl compute | | | 几十字节 | 低 | 高 | 低 |
| 256 MiB | 16 | 下沉 stdin | wrk compute | | | 低 | 低 | 高 | 低 |

关键计算：

```text
网络节省比例 = 读再计算客户端下载量 / 下沉计算客户端下载量
端到端加速比 = 读再计算耗时 / 下沉计算耗时
stdin 优化比例 = tempfile 耗时 / stdin 耗时
```

## 13. 常见问题

### 13.1 CosBench 页面打不开

检查：

```bash
./start-all.sh
lsof -i:19088
tail -f log/system.log
```

### 13.2 CosBench 连接 S3 失败

检查：

```bash
curl http://192.168.0.9:8333/
tail -f /home/dess/s3.log
```

确认 XML 中：

```text
endpoint=http://192.168.0.9:8333
path_style_access=true
accesskey=admin
secretkey=secret
```

### 13.3 compute 返回多 chunk 不支持

说明文件被 Filer 拆成多个 chunk。需要：

```text
1. 重启 Filer，加 -maxMB=1024
2. 重新上传测试文件
3. 再执行 ?compute=sum
```

### 13.4 compute 权限 denied

在三台 volume 节点执行：

```bash
chmod +x /home/dess/compute_program/*.sh
chmod 755 /home/dess/compute_program
```

