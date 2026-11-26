# data 目录说明

本目录下**仅以下两个文件入库**，它们都是**为演示与测试合成的假数据**：

| 文件 | 用途 | 性质 |
|---|---|---|
| `samples/boss.raw.json` | 模拟 BOSS 直聘 `joblist.json` 的响应结构，用于 `--mode=local` 验证「响应解析 → 字段映射」链路 | 结构照真实接口编排；**所有岗位、薪资、招聘者、`encryptJobId` 均为虚构** |
| `jobs.sample.json` | 已是 `model.Job` 结构的示例岗位，用于 `--mode=sample` 零网络自检与前端离线兜底 | 由上一个文件推导并补全，**同样为虚构** |

关于公司名：为了让界面看起来接近真实使用场景，示例中使用了若干**公开知名企业的名称作为占位**，
但对应的职位、薪资、招聘者、福利标签、发布时间**全部是编造的，不代表任何企业真实在招岗位**，
也不构成招聘信息。请勿将其当作真实招聘数据引用或传播。

以下文件属于**运行产物，已被 `.gitignore` 排除，不会入库**：

- `jobs.json`：`collect` 的采集结果，可能包含真实抓取的岗位与公司信息；
- `collect.report.json`：最近一次采集的可观测报告（含查询词、城市、各平台耗时与错误码）。

如果你要在本地复现演示数据：

```bash
go run . collect --mode=local --file=data/samples/boss.raw.json --platforms=boss --query=Java --city=北京
```
