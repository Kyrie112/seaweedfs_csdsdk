# SeaweedFS CSD 计算下沉对比实验报告

## 1. 任务与结论摘要

- 拉取并静态编译了 `Kyrie112/seaweedfs_csdsdk`,生成不依赖 glibc/发行版的 `weed` 可执行文件(144MB,`statically linked`)。
- 在三台 SmartSSD 机器(192.168.0.9/10/11,用户 dess)上部署了 3 节点分布式 SeaweedFS(1 master + 1 filer + 3 volume server),并开启 CSD 计算下沉(volume 端执行 `/home/dess/compute_program` 下的脚本)。
- 验证了 `Sum.sh` 在三个存储节点均可正常执行(测试文件 8 个数,均返回 220)。
- 端到端验证 `?compute=sum` 大文件(176MB,36,000,000 个数)返回正确结果 179986461084。
- **两组对比实验结论**:
  - 主机端计算在 CPU 争抢或网络带宽受限时性能显著下降(无限制 11.2s → 单核+满载 23.6s;10Mbps 下 148.3s)。
  - 计算下沉在相同限制条件下保持 ~17.5s 不变,带宽受限时加速比达 8.4x,叠加 CPU 争抢时达到 **9.09x**(8 次重复,变异系数 <1%)。
  - **最佳配置:主机 CPU = 1 核且被 119 个满载进程争抢 + 网络带宽 = 10Mbps(1.25MB/s)**,此时主机端 159.3s vs 计算下沉 17.5s。

## 2. 部署

### 2.1 二进制

- 源码:`/home/tan/new_workspace/seaweedfs_csdsdk`(commit 412437955,包含 volume compute 实现)
- 构建:CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build(静态链接,无 glibc 依赖)
- 产物:`/home/tan/new_workspace/weed_static`(144MB),分发到三台机器 `/home/dess/seaweed/weed`

### 2.2 集群布局

| 节点 | 角色 | 端口 | 关键参数 |
|---|---|---|---|
| 192.168.0.9 (tan09) | master + filer + volume | 9333 / 8888 / 8080 | filer `-maxMB=1024`;volume 开计算下沉 |
| 192.168.0.10 (tan10) | volume | 8080 | `-volume.compute.enabled=true` |
| 192.168.0.11 (tan11) | volume | 8080 | 同上 |

volume 公共参数:

```bash
-volume.compute.enabled=true
-volume.compute.dir=/home/dess/compute_program
-volume.compute.timeout=300s
-volume.compute.maxOutputMB=16
```

### 2.3 长计算请求的 filer 空闲超时(重要)

计算下沉期间客户端与 filer 之间没有数据流动,而 filer HTTP 监听器带 **10 秒"无活动超时"**,超过 10 秒的计算请求会被断开(volume 日志表现为 `signal: killed`)。本次部署曾以本地补丁把该超时提升到 10 分钟;上游随后已把该值改为可配置参数:

```bash
weed filer ... -idleTimeout=600   # 连接空闲秒数,默认 10;长计算下沉请求需调大
```

调大后,176MB 大文件计算下沉从"10 秒被掐断"变为"17.6 秒正常返回"。
## 3. Sum.sh 验证

三台机器直接执行 `/home/dess/compute_program/sum.sh`(输入 8 个数 3,10,17,24,31,38,45,52):

```text
tan09: 220  tan10: 220  tan11: 220   (期望 220)
```

经 SeaweedFS 端到端:`curl ".../dataset/numbers.txt?compute=sum"` → `688`;非法操作 `?compute=../x` → HTTP 400 拒绝。

## 4. 实验方法

- 数据集:176MB 文本文件,36,000,000 个整数(0..9999,种子固定),期望和 179986461084。
- 主机端计算:客户端 `curl` 从 filer 下载文件,再本地 `awk '{s+=$1}'` 求和;端到端时间 = 下载 + 计算。
- 计算下沉:客户端 `curl "...?compute=sum"`,存储节点(volume)执行 sum.sh,只回传结果。
- 资源限制(客户端):
  - CPU 档位:none(不限)/ 2c(taskset 0-1)/ 1c(taskset 0)/ 1c_burn(taskset 0 + 119 个满载进程争抢)
  - 带宽档位:unlim / 50Mbps(6.25MB/s)/ 20Mbps(2.5MB/s)/ 10Mbps(1.25MB/s),用 curl `--limit-rate` 控制
- 4×4=16 组配置 × 3 次重复 × 2 模式 = 96 次测量;最佳配置额外 5 次重复(合计 8 次)。
- 指标:端到端耗时(mean/std/CV)、结果正确性(所有 96+20 次全部正确)。

## 5. 完整结果(3 次重复均值,秒)

格式:`host/offload(speedup)`

| CPU\带宽 | unlim | 50Mbps | 20Mbps | 10Mbps |
|---|---|---|---|---|
| none | 11.2/17.6 (0.64x) | 36.1/17.5 (2.07x) | 78.1/17.5 (4.47x) | 148.3/17.6 (8.43x) |
| 2c | 11.2/17.4 (0.64x) | 35.9/17.4 (2.06x) | 78.1/17.4 (4.49x) | 148.2/17.8 (8.31x) |
| 1c | 11.0/17.5 (0.63x) | 35.9/17.3 (2.07x) | 77.9/17.4 (4.49x) | 148.2/17.6 (8.43x) |
| 1c_burn | 23.6/17.5 (1.35x) | 46.2/17.4 (2.66x) | 88.4/17.5 (5.06x) | 158.6/17.6 (9.03x) |

要点:

- 无限制时主机端更快(11.2s vs 17.6s),因为下沉多一次网络往返且存储端 awk 与本机速度相当。
- 单独 CPU 争抢(1c_burn × unlim):主机端 23.6s,下沉 17.5s,下沉首次反超(1.35x)。
- 带宽受限主导:50M→~2.1x,20M→~4.5x,10M→~8.4x;CPU 争抢叠加带宽受限进一步放大优势。

## 6. 最佳配置确认(合计 8 次重复)

| 配置 | 主机端 (s) | 计算下沉 (s) | 加速比 | host CV | offload CV |
|---|---|---|---|---|---|
| **1c_burn × 10Mbps** | **159.319 ± 1.112** | **17.520 ± 0.147** | **9.093x** | 0.70% | 0.84% |
| 1c × 10Mbps | 148.257 ± 0.036 | 17.469 ± 0.137 | 8.487x | 0.02% | 0.78% |

全部 8 次重复结果均正确,CV 均 <1%,稳定可复现。

## 7. 结论

1. 主机 CPU 被大量占用或网络带宽不足时,主机端计算性能明显下降(实验中最大恶化约 13 倍:11.2s → 148s,带宽 10Mbps)。
2. 计算下沉将计算放到存储节点执行,传输量从 176MB 降到 10 字节左右,因此对带宽/CPU 限制几乎不敏感(始终 ~17.5s)。
3. 在 **CPU 满载争抢 + 10Mbps 带宽** 的组合下,计算下沉收益最大且最稳定:**9.09x 加速(8 次重复,CV<1%)**,推荐作为该场景的配置。

## 8. 复现

- 实验脚本:`../scripts/run_experiments.sh`(all/one 两种模式)
- 原始数据:`../results/results.csv`(96 行)+ `../results/confirm_*.csv`(4×5 行)
- 分析脚本:`../scripts/analyze_results.py`、`../scripts/final_stats.py`
- 数据文件:`/home/tan/new_workspace/work/big_numbers.txt`(176MB)
