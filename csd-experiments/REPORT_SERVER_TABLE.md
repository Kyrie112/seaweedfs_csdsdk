# Server 端 CPU × 带宽限制对计算下沉的影响(修正版表格实验)

## 1. 目的

承接"计算下沉后压力集中到 Server 端"的分析:限制 **Server 端 CPU 与上传带宽**,
测量其对**计算下沉(offload)时延**的影响,验证 Server 是否成为新的系统瓶颈,
为引入 CSD 设备分担计算提供实验依据。

## 2. 方法(含重要修正)

- 数据:big_numbers.txt(176MB,3600 万个整数),位于 192.168.0.10 volume 3;
  offload = `?compute=sum` 端到端时延(结果 ~10B)。
- **Server CPU 限制用 cgroup CPUQuota**(3200%=32 核 / 200%=2 核 / 100%=1 核):
  实测发现 `taskset` 绑核对 Go 编写的 volume 无效(worker 线程与子进程不继承主线程亲和,
  awk 子进程亲和仍是全核),故改用 systemd `CPUQuota` 这一真正生效的限制方式。
- **Server 上传带宽限制用 tc tbf 在 filer(.9)eno1 出口精确限速**(50/20/10 mbit,
  20Mbps 实测 19.1Mbps),比 filer 内置 `-downloadMaxMBps` 的整数 MB/s 更贴近目标档位。
- **1 Core+争抢 = CPUQuota 100% + 32 个 systemd 持久 burner 占满 32 核**
  (burner 用 systemd-run 启动为持久服务,ssh 断开会话不会误清)。
- 修正了重启 volume 时的端口竞态(旧进程优雅退出需 ~10s,新进程需等 8080/18080 释放再启动,
  否则绑定失败直接崩溃——此前多轮"争抢即崩溃"的假象源于此 bug)。
- 每格 3 次重复(1 Core+争抢 为 2 次),校验返回和=179986461084 才算成功。

## 3. 结果(计算下沉时延,秒)

| Server CPU\带宽 | 无限制 | 50Mbps | 20Mbps | 10Mbps |
|---|---|---|---|---|
| 无限制(32核) | 17.7 | 17.7 | 17.9 | 17.8 |
| 2 Core(200%) | 17.6 | 17.6 | 17.5 | 17.5 |
| 1 Core(100%) | 18.1 | 17.8 | 17.8 | 17.9 |
| 1 Core+争抢(100%+32burner) | 失败(8~34s) | 失败(6~33s) | 失败(7~34s) | 失败(7~36s) |

失败模式:请求返回 `compute operation "sum" failed: signal: killed`(首轮 ~33-36s)或
`context canceled`(第二轮 ~6-8s),未返回正确结果。

## 4. 结论

1. **单请求下沉对 Server 带宽与 CPU 配额(低至 1 核)均不敏感**(~17.5-18s 恒定):
   结果仅 ~10B,带宽限不到;awk 单线程,1 核配额也够。
2. **Server 成为新瓶颈的临界点在 CPU 争抢**:1 核配额 + 32 核满载争抢时,当前下沉
   直接失败、服务不可用——计算与数据处理压力集中到 Server 端后,Server 本身就是瓶颈。
3. 结合此前实验形成完整论证链:
   - 主机 CPU/网络受限 → 下沉有效(9.09x);
   - Server 带宽受限 → 必须近存计算(10.18x);
   - Server CPU 争抢 → 当前下沉失败;
   - **只有 CSD(设备内引擎,不占 Server CPU、只回传结果)能同时绕开 Server 带宽与 CPU 瓶颈。**

## 5. 复现与数据

- 脚本:`run_server_table2.sh`(含端口等待修复)
- 原始数据:`results_server_table.csv`(44 组)
- 表格生成:`make_server_table.py`
