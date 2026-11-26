# 求职工作台（job-workspace）

一个**单二进制、零第三方依赖**的本地求职工作台：可插拔地采集招聘平台职位数据，落盘为统一结构，
并通过内置 Web 界面做检索、筛选、收藏与投递跟进。

- 采集侧：`collect` 子命令，支持 `sample`（离线示例）/ `local`（离线导入原始响应）/ `live`（受控联网）三种模式
- 展示侧：`serve` 子命令，启动本地服务并打开单文件工作台
- 接入侧：新增招聘平台 = **新增一个适配器包 + 一段配置**，核心业务流程零改动

> 视觉风格参考 Apple 官网的设计语言（留白、层级、克制配色、细节动效），仅借鉴风格，未使用其任何品牌文案或素材。

**仓库内容边界**：本仓库**不含**任何账号凭据、Cookie、令牌或真实个人数据。
`data/` 下入库的两个样例文件是**合成假数据**（详见 [data/README.md](data/README.md)）；
采集产物 `data/jobs.json`、`data/collect.report.json` 与编译产物一律被 `.gitignore` 排除。

```bash
git clone git@github.com:qiubite666/job-workspace.git
```

---

## 1. 快速开始

要求 Go 1.22+（实测 1.26.4），**无需 `go mod tidy`、无需联网下载依赖**。

```bash
cd job-workspace

# 1) 先创建后台账号（首次必须做，否则无法登录）
go run . user add --username admin
# 会打印一次性随机密码，请保存

# 2) 启动工作台
go run . serve
# 浏览器打开 http://127.0.0.1:8787，用上面的账号登录
```

编译为单文件二进制：

```bash
go build -o jobworkspace.exe .
./jobworkspace.exe serve
```

---

## 2. 目录结构

```
job-workspace/
├── main.go                       CLI 入口：collect / serve / platforms / doctor
├── go.mod                        模块 jobworkspace，零第三方依赖
├── contracts/CONTRACT.md         接入层接口契约（唯一真源）
├── internal/
│   ├── model/job.go              统一岗位模型 Job / JobDraft / Report
│   ├── errs/errors.go            错误码与异常归一化
│   ├── parser/salary.go          薪资原文解析
│   ├── parser/time.go            发布时间解析
│   ├── mapping/mapper.go         声明式字段映射 + 收口
│   ├── dedupe/dedupe.go          ID / 指纹两级去重
│   ├── ratelimit/limiter.go      全局串行限速
│   ├── httpx/client.go           限速 + 重试 + 超时
│   ├── httpx/robots.go           robots.txt 网关
│   ├── config/config.go          配置加载与默认值回填
│   ├── platform/registry.go      Adapter 接口 + 注册表（唯一扩展点）
│   ├── platform/httpjson/        可复用的「HTTP-JSON 平台」引擎
│   ├── platform/boss/            BOSS 直聘适配器
│   ├── platform/liepin/          猎聘适配器（骨架，默认禁用）
│   ├── platform/template/        复制即用的新平台骨架
│   ├── platform/catalog/         适配器工厂与装配
│   ├── pipeline/collect.go       采集编排（平台无关）
│   ├── store/store.go            data/*.json 原子读写与合并
│   └── web/server.go + index.html  本地服务 + 单文件工作台
├── config/platforms.json         平台配置（运行时可直接复制修改，用 --config 覆盖）
└── data/
    ├── jobs.json                 采集产物（运行时生成，不入库）
    ├── collect.report.json       采集报告（运行时生成，不入库）
    ├── users.json                后台账号（含密码哈希，不入库）
    └── userstate/                每个账号的收藏/投递状态/备注（不入库）
```

---

## 3. 分层架构

```
┌──────────────────────────────────────────────────────────────┐
│  展示层   internal/web  （单文件 HTML，内联 CSS/JS/SVG）        │
├──────────────────────────────────────────────────────────────┤
│  编排层   internal/pipeline      平台无关：翻页/降级/统计        │
│           internal/store         原子落盘 + 按 ID 合并           │
├──────────────────────────────────────────────────────────────┤
│  能力层   parser（薪资/时间） mapping（字段映射） dedupe（去重）  │
│           httpx（限速/重试/robots） ratelimit                   │
├──────────────────────────────────────────────────────────────┤
│  接入层   platform.Adapter 接口 + 注册表                        │
│           ├── httpjson  通用引擎（配置驱动）                    │
│           ├── boss      BOSS 直聘                              │
│           └── liepin    猎聘（骨架）                            │
└──────────────────────────────────────────────────────────────┘
```

**调用链单向**：展示 → 编排 → 能力 → 接入。接入层只依赖 `model/errs/config/httpx/mapping`，
**不反向依赖 pipeline 或 web**；新增平台不会触碰任何上层代码。

---

## 4. 统一数据结构

所有平台最终收敛为 `model.Job`，字段同名同义。关键字段：

| 字段 | 说明 |
|---|---|
| `id` | `平台:平台ID`；无平台 ID 时 `平台:sha1(公司\|岗位\|城市\|区域)前16位` |
| `salaryDesc` | 薪资原文，永不改写 |
| `salaryMin/Max` | 原始单位下的数值；`null` 表示未知 |
| `salaryUnit` | `month` / `day` / `year` / `""` |
| `salaryKind` | `range` / `single` / `negotiable` / `unknown` |
| `salaryMonthlyMin/Max` | 归一到「元/月」，用于跨单位排序（日薪按 21.75 天/月） |
| `publishedAt` | RFC3339 或 `""`；`publishedAtSource` 标注 `api/detail/missing` |
| `jobUrl` | 详情页链接 |

缺值约定：字符串 `""`、数值 `null`、数组 `[]`——前端因此可以安全地判空，不会出现 `undefined`。

---

## 5. 采集（仅 live 模式）

v2 起只保留联网采集一种方式，`sample` / `local` 模式已移除——它们产生的数据来源混杂，
容易让人把演示数据当成真实结果。

| 项目 | 说明 |
|---|---|
| 网络 | 是（真实请求平台搜索接口） |
| 前置条件 | 自备已登录的 cookie（`--cookie` / `--cookie-file` / `BOSS_COOKIE`） |
| 落盘 | `data/jobs.json`，与已有数据按 ID 合并去重 |

### 合规边界（重要，默认拦截）

`https://www.zhipin.com/robots.txt` 含 `Disallow: /*?*`，**所有带查询参数的 URL 都被声明禁止抓取**，
搜索接口正在此列。因此：

1. 内置 robots 网关命中 `Disallow` 即抛 `ROBOTS_DISALLOWED` 并停止，**默认不放行**。
2. 只有显式传入 `--allow-disallowed`（界面里是一个默认不勾选的复选框）才会放行，并打印风险提示。
3. 请求间隔强制 `3~6s` 随机抖动、单平台最多 10 页、失败按 800ms×2ⁿ 退避且仅重试可重试错误。
4. 工具不绕过验证码、不破解登录、不伪造指纹。
5. **不提供本地导入模式后，若所在场景不允许抓取，请直接在浏览器里查看岗位，不要使用本工具的采集功能。**

> 需要说明的取舍：移除 `local` 模式后，默认配置下 `collect` 会因 robots 拦截而失败，
> 必须显式加 `--allow-disallowed` 才会真正发出请求。这是刻意的——让"越界"成为一个需要主动确认的动作。

---

## 6. CLI 参考

```
jobworkspace collect [flags]
  --query <kw>            关键词
  --city <name|code>      城市，默认 北京
  --pages <n>             页数，默认取配置，上限 maxPages
  --page-size <n>         每页条数
  --platforms a,b         仅跑指定平台
  --cookie <str>          live 模式登录态（也会读 --cookie-file 与 BOSS_COOKIE）
  --cookie-file <path>    从文件读取登录态 cookie，部署场景推荐
  --allow-disallowed      显式放行 robots 禁止路径（不加则命中即失败）
  --out <path>            默认 data/jobs.json
  --config <path>         外部配置，默认用内嵌 platforms.json
  --json                  以 JSON 输出摘要
  --quiet                 只输出错误

jobworkspace serve  [--addr 127.0.0.1:8787] [--data data] [--cookie-file <path>]
jobworkspace platforms
jobworkspace doctor
jobworkspace user   add|list|passwd|delete
```

退出码：`0` 成功 / `1` 参数或配置错误 / `2` 全部平台失败 / `3` 部分平台失败。

### 登录态 cookie 的三种来源

优先级：`--cookie` > `--cookie-file` > 环境变量 `BOSS_COOKIE`。

| 方式 | 适用 | 注意 |
|---|---|---|
| `--cookie-file /path/boss.cookie` | **部署推荐** | 文件内容就是整行 Cookie 值（`k=v; k2=v2`），权限 0600；不进入环境变量，也不会出现在命令行、`ps`、`/proc/<pid>/environ` |
| 环境变量 `BOSS_COOKIE` | 临时本机使用 | 注意 shell 历史与 `systemctl show -p Environment` 都可能暴露 |
| `--cookie` 直传 | 仅调试 | 会留在 shell 历史里 |

cookie 形如单行：

```
lastCity=101080100; ab_guid=...; wt2=...; zp_at=...; __zp_stoken__=...; bst=...; __c=...; __a=...
```

即从浏览器 DevTools「Network → 请求头 → cookie」整行复制，不要加 `Cookie:` 前缀，不要加引号，不要拆成多行。

**有效期**：`wt2` / `zp_at` / `bst` 通常数天到数周，`__zp_stoken__` 是短周期令牌（由站点 JS 计算并刷新，小时级到天级）。
服务端**无法自动续期**——续期依赖站点自身的 JS SDK，只有在浏览器环境里才会发生。失效后采集会返回 `AUTH_REQUIRED`，需要重新导出 cookie。

---

## 7. HTTP API

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/` | 公开 | 工作台页面 |
| GET | `/admin` | 公开（页面内置登录框） | 后台管理页面 |
| GET | `/api/health` | 公开 | 健康检查、岗位数、数据源 |
| GET | `/api/jobs` | 公开 | 岗位列表 + 元信息（含最近一次采集报告） |
| GET | `/api/platforms` | 公开 | 平台清单与城市码表 |
| POST | `/api/collect` | 登录 | 触发采集（单飞锁，并发返回 409） |
| POST | `/api/auth/login` | 公开 | 登录，成功下发 `wb_sid` Cookie |
| POST | `/api/auth/logout` | 公开 | 退出（幂等） |
| GET | `/api/auth/me` | 公开 | 探测登录态，未登录返回 `authenticated:false` |
| GET | `/api/admin/jobs` | 登录 | 后台岗位列表（含搜索） |
| GET | `/api/admin/jobs/{id}` | 登录 | 单个岗位 |
| POST | `/api/admin/jobs` | admin | 新增岗位（`platform=manual`） |
| PUT | `/api/admin/jobs/{id}` | admin | 编辑岗位 |
| DELETE | `/api/admin/jobs/{id}` | admin | 删除岗位 |

服务只监听 `127.0.0.1`，不对外暴露。

**鉴权说明**：`v1.1.0` 起 `/api/collect` 需要登录——它会真实发起对外请求，公开暴露会牵连服务器 IP。
其余只读接口保持公开，工作台前台使用不受影响。

---

## 8. 登录与后台管理（v1.1.0）

### 8.1 首次创建管理员

服务启动时若 `data/users.json` 为空，日志会给出提示。创建账号：

```bash
./jobworkspace user add --username admin --name 管理员 --role admin
# 未指定 --password 时会生成 16 位随机密码，只打印一次
```

| 子命令 | 说明 |
|---|---|
| `user add --username x [--role admin\|viewer] [--password x]` | 新建账号 |
| `user list` | 列出账号（不含哈希） |
| `user passwd --username x` | 重置密码 |
| `user delete --username x` | 删除账号（拒绝删除最后一个管理员） |

### 8.2 认证实现

- **密码哈希**：PBKDF2-HMAC-SHA256，210000 次迭代、16 字节盐、32 字节密钥，编码
  `pbkdf2-sha256$210000$<saltB64>$<hashB64>`；比对使用 `crypto/subtle` 恒定时间比较。
  为保持零第三方依赖，未引入 `golang.org/x/crypto`，实现以 RFC 2898 标准向量做单测。
- **会话**：服务端内存会话（`crypto/rand` 32 字节 ID）+ `HttpOnly` Cookie `wb_sid`，
  `SameSite=Lax`，TTL 12h，剩余不足一半自动续期。**进程重启会话失效**（单实例设计）。
- **每请求复核**：不仅校验会话，还会回查用户当前状态——被删除/禁用立即踢下线，
  角色被改动立即生效（降级为 viewer 后写接口立刻 403）。
- **登录限流**：同一 `(客户端IP, 用户名)` 连续失败 5 次锁定 60 秒，返回 429。
- **权限模型**：`admin` 可增删改；`viewer` 只读。写接口统一走 `requireRole(admin)`。

### 8.3 后台能力

- 岗位列表：分页、按职位/公司/城市搜索
- 新增 / 编辑：服务端字段级校验（必填、长度、URL 协议），失败返回 422 + 字段错误映射
- 删除：二次确认后不可撤销
- 手工录入的岗位 `platform=manual`，与采集数据同表共存，不影响采集去重
- 薪资原文由 `parser.ParseSalary` 解析成数值，与采集链路口径一致

---

## 9. 新增一个招聘平台（3 步）

1. 复制骨架：`cp -r internal/platform/template internal/platform/xyz`
2. 在 `xyz` 包里实现 `New(p *config.Platform) platform.Adapter`，若接口与 `httpjson` 一致，
   直接 `return httpjson.New(httpjson.SpecFromConfig(p))`，平台差异写进配置即可。
3. 在 `config/platforms.json` 增加一条 `xyz` 配置，并在 `main.go` 的 `initAdapters()` 里加一行
   `xyz.RegisterFactory()`。

**不需要**改 `pipeline`、`store`、`web`、`model`、`dedupe`、`parser` 中任何一行——这就是本次重构的验收标准。

---

## 9. 边界条件与错误处理

| 场景 | 行为 |
|---|---|
| 单平台 401/403 | 归一 `AUTH_REQUIRED`，停止该平台翻页，不影响其他平台 |
| 触发限频 / 风控（code 31/37） | 归一 `RATE_LIMITED` / `BLOCKED`，停止该平台，报告记录错误码 |
| robots 禁止 | `ROBOTS_DISALLOWED`，未显式放行则不发请求 |
| 单页超时 | `TIMEOUT`，重试 2 次后放弃该页，平台标记 `failed` |
| 响应结构变化 | `PARSE_FAILED`，丢弃该条并计入 `dropped`，其余条目继续 |
| 列表为空 | 记 `EMPTY` 并停止翻页，不视为错误 |
| 薪资为「面议」/空 | `salaryKind=negotiable/unknown`，数值为 `null`，岗位照常入库 |
| 发布时间缺失 | `publishedAt=""`、`publishedAtSource="missing"`，排序时排在有时间的之后 |
| 重复岗位（同 ID / 同指纹） | 保留完整度最高者，计入 `duplicates` |
| 数据文件不存在 | 返回空列表（无示例数据回退），前端显示空态，不报错 |
| 采集子进程超时 | HTTP 500，返回 stderr 末 2000 字符 |

---

## 10. Windows 使用

从 [Releases](https://github.com/qiubite666/job-workspace/releases/latest) 下载
`jobworkspace-windows-amd64.exe`，无需安装、无需 Go 环境。

```powershell
# 进入放置目录
cd D:\job-workspace

# 创建管理员（首次必须做；不指定 --password 会生成随机密码并只打印一次）
.\jobworkspace-windows-amd64.exe user add --username admin

# 启动工作台
.\jobworkspace-windows-amd64.exe serve
# 浏览器打开 http://127.0.0.1:8787
```

实用说明：

- 服务**只绑定 `127.0.0.1`**，不监听局域网，也不主动开防火墙端口；只有本机能访问。
- PowerShell 里路径含空格要加引号：`& "D:\my tools\jobworkspace.exe" serve`。
- 需要长期运行时，用「任务计划程序」在系统启动时执行；本机不提供 Windows 服务安装脚本。
- 数据默认落在**当前工作目录**的 `data\`，所以先 `cd` 到固定目录再启动，否则换目录就看不到数据。
  也可以用 `--data D:\job-workspace\data` 指定绝对路径。
- cookie 文件同 Linux：`--cookie-file D:\job-workspace\boss.cookie`，单行整段值。

---

## 11. 全站登录与数据云同步（v2）

- **整站登录**：首页与 `/admin` 共用同一会话。未登录时首页只显示登录卡片，
  不加载任何岗位数据；用 `admin` 登录后可直接在顶栏看到「管理后台」入口，无需二次登录。
- nginx 不再做 Basic Auth，鉴权全部收敛到应用层（nginx 只负责 TLS 与反向代理）。
- `/api/jobs`、`/api/collect`、`/api/admin/*`、`/api/me/state` 均要求登录；
  `/api/health`、`/api/platforms` 保持公开（监控探活与静态配置）。
- **收藏 / 投递状态 / 备注云端化**：按账号隔离，落在 `data/userstate/<用户名>.json`（0600）。
  接口为 `GET/PUT /api/me/state`，服务端会归一化（去重、阶段白名单、备注 500 字上限）
  并忽略客户端伪造的 `version` / `updatedAt`。
- 前端变更后 800ms 防抖自动保存，顶栏显示「已保存 / 保存中 / 保存失败」并支持「立即保存」。
  退出登录会清空内存态，避免串号。

---

## 12. 已知限制与命名债

- **命名债**：统一模型中的 `bossName` / `bossTitle` 语义是「招聘者姓名 / 招聘者职位」，
  沿用了 BOSS 直聘的字段词。这是本次统一建模时的历史命名，属于**平台词汇泄漏进核心模型**的瑕疵。
  改名方案（向后兼容）：`RecruiterName` / `RecruiterTitle`，JSON 同时输出
  `recruiterName` 与保留别名 `bossName`，前端逐步切到新字段后即可删除旧字段。
  本次未做，是因为改名要同时触及 model / mapping / 前端三处，会与并行开发的前端任务冲突。
- BOSS 直聘列表接口通常**不返回发布时间**，只有部分条目带 `postTime`；缺失时前端显示「时间未提供」，
  并明确标注来源，不做推断造假。详情页可补，但需要登录态且请求量翻倍，本期未实现。
- `liepin` 适配器是**骨架**（`experimental: true`、默认 `enabled: false`），只保证接口正确与可编译，不保证可采。
- 单机单用户设计：无账号体系，收藏/投递状态存浏览器 `localStorage`，靠 JSON 导出导入迁移。
- 未做分布式去重与增量调度；每次采集为全量抓取后按 ID 合并。
