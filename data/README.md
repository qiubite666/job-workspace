# data 目录说明

本目录只存放**运行产物**，全部已被 `.gitignore` 排除，不入库：

| 文件 | 用途 |
|---|---|
| `jobs.json` | `collect` 的采集结果（按 ID 合并写入），可能包含真实抓取的岗位与公司信息 |
| `collect.report.json` | 最近一次采集的可观测报告（含查询词、城市、各平台耗时与错误码） |

采集只有联网一条路径，仓库不再内置任何示例数据或抓包文件：
`jobs.json` 不存在时 `LoadJobs` 返回空列表（前端展示空态），不会再回退到示例数据。

首次使用请直接跑一次真实采集：

```bash
go run . collect --query=Java --city=北京 --pages=2 --platforms=boss
```

无 cookie 时 BOSS 会返回 `AUTH_REQUIRED`，请通过 `--cookie` / `--cookie-file` /
环境变量 `BOSS_COOKIE` 提供登录态；robots.txt 默认拦截，确需放行时显式加 `--allow-disallowed`。
