# CSD 计算实验与文档目录

本目录是 SeaweedFS + SmartSSD 可计算存储工作的实验、文档与结果归档。

## 创新说明

面向论文的创新点总述与论证见 [INNOVATIONS.md](INNOVATIONS.md)。

## 目录结构

```text
csd-experiments/
├── INNOVATIONS.md            # 相对上游 SeaweedFS 的创新点说明
├── reports/                  # 性能实验与方案报告
├── guides/                   # 多接口使用指南
├── scripts/                  # 实验驱动、分析与数据生成脚本
├── results/                  # 实验原始数据 CSV
└── iterations/               # V0-V4 迭代记录与 P2P/XRT 对比文档
```

## 内容索引

### 创新点

- [INNOVATIONS.md](INNOVATIONS.md)

### 报告(reports/)

- [REPORT.md](reports/REPORT.md):主机计算 vs 计算下沉全网格实验
- [REPORT_CROSS_CHUNK.md](reports/REPORT_CROSS_CHUNK.md):跨 chunk 计算实现与验证
- [REPORT_MULTIMODAL_INTERFACE.md](reports/REPORT_MULTIMODAL_INTERFACE.md):多模态接口
- [REPORT_CSD_DEPLOY.md](reports/REPORT_CSD_DEPLOY.md):file_sum64 CSD 部署
- [REPORT_CSD_IMPACT.md](reports/REPORT_CSD_IMPACT.md):CSD 争抢影响
- [REPORT_SERVER.md](reports/REPORT_SERVER.md):Server 端资源受限实验
- [REPORT_SERVER_CSD.md](reports/REPORT_SERVER_CSD.md):Server 端 CPU/带宽受限
- [REPORT_SERVER_TABLE.md](reports/REPORT_SERVER_TABLE.md):Server CPU × 带宽时延表

### 使用指南(guides/)

- [GUIDE_MULTI_INTERFACE.md](guides/GUIDE_MULTI_INTERFACE.md):文件/对象/块调用方式

### 脚本(scripts/)

- `run_experiments.sh` / `run_server_experiments.sh`:主实验驱动
- `run_server_cpu_quota.sh` / `run_server_table2.sh`:CPU/带宽限定实验
- `run_csd_contention.sh`:CSD 争抢实验
- `gen_data.sh`:测试数据生成
- `burner.sh`:CPU 争抢负载
- `analyze_*.py` / `final_*.py` / `make_server_table.py`:统计与成表

### 结果(results/)

- `results.csv`:主机 vs 下沉全网格
- `results_server*.csv`:Server 端实验
- `results_csd_contention.csv`:CSD 争抢
- `confirm_*.csv`:最优配置确认

### 版本迭代(iterations/)

- [ITERATION-00](iterations/ITERATION-00_script-baseline.md):脚本+临时文件基线
- [ITERATION-01](iterations/ITERATION-01_multimodal-upper-interface.md):跨 chunk + 多模态
- [ITERATION-02](iterations/ITERATION-02_csd-native-region-dispatch.md):CSD 原生分派
- [ITERATION-03](iterations/ITERATION-03_p2p-near-storage.md):P2P 数据通路
- [ITERATION-04](iterations/ITERATION-04_csd-aware-replica-scheduling.md):CSD-aware 调度
- [COMPARISON_P2P_vs_XRT.md](iterations/COMPARISON_P2P_vs_XRT.md):P2P vs XRT 对比
