# 版本迭代说明文档

本目录按迭代记录 SeaweedFS + SmartSSD 可计算存储从“CPU 脚本”到“CSD 近存计算”
的每次技术演进,供论文写作与复盘使用。

## 迭代索引

| 迭代 | 主题 | 状态 | 文档 |
| --- | --- | --- | --- |
| V0 | 脚本 + 临时文件计算(基线) | 已完成 | [ITERATION-00](ITERATION-00_script-baseline.md) |
| V1 | 跨 chunk + 文件/对象/块多模态上层接口 | 已完成 | [ITERATION-01](ITERATION-01_multimodal-upper-interface.md) |
| V2 | SeaweedFS CSD 原生分派 + SmartSSD agent | 已完成(部分集成) | [ITERATION-02](ITERATION-02_csd-native-region-dispatch.md) |
| V3 | P2P / 盘直通 FPGA | 已完成(agent 侧真机验证) | [ITERATION-03](ITERATION-03_p2p-near-storage.md) |

## 文档维护约定

1. 每完成一个可验证的开发里程碑,新增一篇 `ITERATION-XX_<主题>.md`;
2. 每篇文档必须包含:目的、路径图、代码位置、改动点、验证结果、当前限制、下一迭代;
3. 若新迭代只完成部分集成,状态写“已完成(部分集成)”,并在文档中明确未完成部分;
4. 文档应可独立阅读,论文可直接引用其中图表、路径与数据;
5. 新迭代沿用 [ITERATION-03](ITERATION-03_p2p-near-storage.md) 中的章节模板。
