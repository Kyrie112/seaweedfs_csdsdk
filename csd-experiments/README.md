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

### 一致性

- [../DISTRIBUTED_CONSISTENCY_PLAN.md](../DISTRIBUTED_CONSISTENCY_PLAN.md):分布式一致性问题的挑战分析与开发计划(挑战 C1–C7、方案 5.1–5.7、里程碑 M1–M4)

### 设备侧模块

- [`../smartssd/`](../smartssd/README.md):CSD agent、FPGA 内核、主机侧参考测试(已并入本仓库,`make -C smartssd test`)

### 规划

- [reports/THESIS_DRAFT.md](reports/THESIS_DRAFT.md):**毕业论文初稿**(摘要/绪论/技术基础/总体设计/关键机制/图像检索算子/实验/结论)
- [reports/REPORT_PROJECT_OVERVIEW.md](reports/REPORT_PROJECT_OVERVIEW.md):**项目总览汇报**(面向不了解本工作的读者,含速览/背景/逐条工作/结果/讲稿)
- [ROADMAP.md](ROADMAP.md):后续工作计划(阶段一机制修复 / 阶段二调度模型 / 阶段三算子与实验)
- [tools/csd-agent-sim](tools/csd-agent-sim):无 FPGA 的软件 CSD agent(同协议、同语义)
- [scripts/run_phase1_local.sh](scripts/run_phase1_local.sh):本地部署 + 读路径/自适应策略性能实验
- [scripts/run_image_search_experiment.sh](scripts/run_image_search_experiment.sh):近存以图搜图(top-k)实验
  (图库规模/k 可参数化,内置暴力检索参考实现与传输量对比;
  `SCRIPT=image_search_mnist_experiment.py` 可切换到真实数据集 MNIST)
- [scripts/make_figures.py](scripts/make_figures.py):从结果 CSV 重绘论文图表(Pillow)
- [results/figures/](results/figures):论文图 6-1 ~ 6-7
  (`MODES="direct auto"` 可跑自适应缓存策略对比)

### 创新点

- [INNOVATIONS.md](INNOVATIONS.md)

### 报告(reports/)

- [REPORT_PHASE1_HARDENING.md](reports/REPORT_PHASE1_HARDENING.md):阶段一(P2P 对齐通路 / 短读校验 / 调度加固)实施与验证
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
