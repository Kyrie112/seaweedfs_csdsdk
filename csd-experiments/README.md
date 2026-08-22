# CSD 计算下沉对比实验(SeaweedFS)

本目录是 2026-08-22 在三节点 SeaweedFS(192.168.0.9/10/11,均开启
`-volume.compute.enabled` 计算下沉)上完成的"主机端计算 vs 存储计算下沉"对比实验
完整报告与复现工具。

## 文件

| 文件 | 说明 |
| --- | --- |
| `REPORT.md` | 完整实验报告(部署、方法、16 组配置数据、最佳配置结论) |
| `run_experiments.sh` | 实验驱动脚本(`all` 全网格 / `one` 单配置) |
| `burner.sh` | CPU 满载争抢进程(配合 1c_burn 档位) |
| `analyze_results.py` / `final_stats.py` | 结果统计与最优配置分析 |
| `results.csv` | 全网格原始数据(16 配置 × 3 次重复 × 2 模式) |
| `confirm_*.csv` | 最佳/亚军配置追加 5 次重复数据 |
| `gen_data.sh` | 重新生成 176MB 测试数据集(big_numbers.txt,不入库) |

## 关键结论

- 主机端计算在 CPU 争抢或带宽受限时性能急剧下降(最差 11.2s → 158.6s);
- 计算下沉对资源限制几乎不敏感(~17.5s 恒定);
- **最佳配置:1 核满载争抢 + 10Mbps 带宽,加速比 9.09x(8 次重复,CV<1%)**。

## 复现

```bash
./gen_data.sh                       # 生成 176MB 数据集
# 上传到 SeaweedFS filer:
# curl -F file=@big_numbers.txt http://<filer>:8888/dataset/big_numbers.txt
./run_experiments.sh all results.csv 3   # 全网格,3 次重复
python3 analyze_results.py results.csv   # 汇总统计与加速比
```

脚本中的 URL/路径为本次部署环境,换环境时按需修改
`run_experiments.sh` 顶部的 `URL`/`DL_FILE` 等变量。
