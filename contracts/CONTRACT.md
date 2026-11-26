# 招聘平台接入层 · Go 实现契约 v1.0（历史留档）

> **状态：已被 `contracts/V2.md` 取代，仅作历史参考。**
> 其中关于 sample / local 模式、示例数据回退、nginx Basic Auth 的描述在 v2 均已失效。
> 当前接口真源：优先 `contracts/V2.md`，平台适配器相关章节仍可参考本文。

> 并行开发的唯一接口依据。**已冻结的共享类型**见 `internal/model`、`internal/errs`、`internal/platform`（registry），
> 这三个包由主控编写，任何子 agent **不得修改**。其余文件按下方归属各自实现。

工程根：仓库根目录 `job-workspace/`　模块名：`jobworkspace`　Go：1.22+（实测 1.26.4）
**零第三方依赖**，只用标准库。

---

## 0. 行为基线（重构前后必须一致）

1. 统一岗位模型 `model.Job`，跨平台字段同名同义。
2. 平台内降级链：`fallback` 顺序尝试，单平台失败不阻塞其他平台。
3. 去重：`id` 优先，退化到 `公司|岗位|城市|区域` 指纹。
4. 空结果 / 缺字段 / 单平台异常，都不得让整次采集返回错误。
5. 薪资保留原文，同时给出解析区间；解析失败降级 `unknown`，不丢岗位。

## 1. 目录与文件归属（严禁越界）

| 归属 | 文件 |
|---|---|
| 主控（已写） | `go.mod`、`internal/model/job.go`、`internal/errs/errors.go`、`internal/platform/registry.go`、`data/samples/boss.raw.json`、`data/jobs.sample.json`、`contracts/CONTRACT.md` |
| **G1** | `internal/parser/salary.go`、`internal/parser/time.go`、`internal/parser/parser_test.go` |
| **G2** | `internal/mapping/mapper.go`、`internal/dedupe/dedupe.go`、`internal/dedupe/dedupe_test.go` |
| **G3** | `internal/ratelimit/limiter.go`、`internal/httpx/client.go`、`internal/httpx/robots.go`、`internal/ratelimit/limiter_test.go` |
| **G4** | `internal/config/config.go`、`internal/config/platforms.json`、`internal/platform/httpjson/spec.go`、`internal/platform/httpjson/adapter.go`、`internal/platform/boss/boss.go`、`internal/platform/liepin/liepin.go`、`internal/platform/template/adapter.go`、`internal/platform/catalog/catalog.go` |
| **G5** | `internal/pipeline/collect.go`、`internal/store/store.go`、`main.go` |
| **G6** | `internal/web/server.go`、`internal/web/index.html` |

> 任何跨归属改动必须先说明，不得直接改别人的文件。全部文件 UTF-8 无 BOM。

---

## 2. 已冻结的共享类型（只读，按此编码）

```go
// internal/model
type Job struct{ /* 见文件，字段名固定，前端已按此渲染 */ }
type JobDraft struct{ ExternalID, JobName, BrandName, CityName, AreaDistrict, BusinessDistrict,
    SalaryDesc, Experience, Degree string; JobLabels, Skills, WelfareList []string;
    BossName, BossTitle, ActiveTimeDesc, JobURL, BrandIndustry, BrandScaleName, BrandStageName string;
    PublishedRaw []string; Extra map[string]any }
type Params struct{ Query, City string; Pages, PageSize int; Platforms []string; Mode, File, Cookie string; AllowDisallowed bool }
type ListResult struct{ Items []map[string]any; HasMore bool }
type Attempt struct{ Name string; OK bool; Code, Message string; MS int64 }
type ErrItem struct{ Code, Message string; Page int }
type PlatformStat struct{ ID, Name, Status, UsedStep string; Pages, Fetched, Mapped, Dropped int; Attempts []Attempt; Errors []ErrItem }
type Totals struct{ Platforms, Fetched, Mapped, Dropped, Unique, Duplicates int }
type Report struct{ OK bool; StartedAt, FinishedAt string; DurationMS int64; Params Params; Platforms []PlatformStat; Totals Totals }
type CollectResult struct{ Jobs []Job; Report Report }
type FinalizeContext struct{ Platform, PlatformName, Keyword, QueryCity, CollectedAt string }

// internal/platform
type Adapter interface {
    ID() string
    Meta() Meta
    Capabilities() Capabilities
    NormalizeParams(p model.Params) model.Params
    FetchList(ctx context.Context, rt *Runtime, p model.Params) (model.ListResult, error)
    MapItem(rt *Runtime, raw map[string]any) (model.JobDraft, error)
}
func Register(a Adapter)      // 各平台 init() 调用
func All() []Adapter          // 已按 priority 排序
func Get(id string) (Adapter, bool)
func SetPriorityResolver(fn func(id string) int) // main 注入
```

`platform.Runtime{ Client *httpx.Client; Config *config.Platform; Log *slog.Logger; Now func() time.Time; Cities map[string]string }`

---

## 3. G1 · internal/parser

```go
package parser

type Salary struct {
    Min, Max               *int   // 原始单位下的数值（元）
    Months                 *int   // 年薪月数
    Unit                   string // "month" | "day" | "year" | ""
    Kind                   string // "range" | "single" | "negotiable" | "unknown"
    MonthlyMin, MonthlyMax *int   // 归一到元/月，用于排序
}

func ParseSalary(text string) Salary
func FormatSalary(s Salary) string          // 全空 → "薪资未标注"
func MonthlyRange(s Salary) (min, max *int) // 仅当 Kind 为 range/single 时返回

// ParsePublishTime 解析发布时间候选值（按优先级），返回 RFC3339(UTC) 或 ""。
// source 取 "api"（解析成功）或 "missing"（全部失败）。
func ParsePublishTime(candidates []string, now time.Time) (iso string, source string)

// RelativeTime 把 ISO 时间渲染为「今天/昨天/3天前/2026-11-24/未提供」。
func RelativeTime(iso string, now time.Time) string

// DayDiff 返回 b-a 的整数天，空值返回 nil（用于判断逾期跟进）。
func DayDiff(aISO, bISO string) *int
```

`ParseSalary` 必须覆盖的用例（写进 `parser_test.go`）：

| 输入 | Min | Max | Months | Unit | Kind | MonthlyMin | MonthlyMax |
|---|---|---|---|---|---|---|---|
| `20-35K·14薪` | 20000 | 35000 | 14 | month | range | 20000 | 35000 |
| `15-25K` | 15000 | 25000 | nil | month | range | 15000 | 25000 |
| `30K-60K` | 30000 | 60000 | nil | month | range | 30000 | 60000 |
| `1-1.5万` | 10000 | 15000 | nil | month | range | 10000 | 15000 |
| `8000-12000` | 8000 | 12000 | nil | month | range | 8000 | 12000 |
| `20K以上` | 20000 | nil | nil | month | single | 20000 | 20000 |
| `300-500元/天` | 300 | 500 | nil | day | range | 6525 | 10875 |
| `面议` | nil | nil | nil | nil | unknown→negotiable | nil | nil |
| `""` | nil | nil | nil | nil | unknown | nil | nil |
| `薪资保密` | nil | nil | nil | nil | unknown | nil | nil |

换算规则：`元/天 → ×21.75`；`万/月 → ×10000`；`万/年 → ×10000/12`；裸数字默认按月。
`·13薪`/`×14薪`/`14薪` 均识别为 Months。全角字符需先规整。

`ParsePublishTime` 支持：13 位毫秒/10 位秒时间戳、`2026-11-24`、`2026-11-24 10:30`、`2026/11/24`、
`11-24`（无年份：若推算日期比 now 晚 30 天以上，判为去年）、`今天/昨天/前天/刚刚`、`N分钟前/N小时前/N天前`、
ISO8601。结果必须落在 `[now-3年, now+1天]`，越界视为无效并继续尝试下一个候选。

## 4. G2 · internal/mapping 与 internal/dedupe

```go
package mapping

type Spec struct {
    Platform         string
    PlatformName     string
    FieldMap         map[string][]string // 字段 -> 候选路径（点路径，按序取第一个非空）
    PublishTimePaths []string
    JobURLTemplate   string              // 支持 {{externalId}}
}
type Context struct{ Keyword, QueryCity, CollectedAt string }

func ReadPath(obj map[string]any, path string) (any, bool)
func ReadString(obj map[string]any, path string) string
func ReadStrings(obj map[string]any, path string) []string
func BuildDraft(raw map[string]any, spec Spec) model.JobDraft
func Finalize(d model.JobDraft, spec Spec, ctx Context) model.Job
func Fingerprint(j model.Job) string
func MakeID(platform, externalID, fp string) string
func Completeness(j model.Job) int   // 0~100
func NormalizeText(s string) string  // trim + 全角转半角 + 压缩空白
```

- `ReadPath` 支持 `a.b.0.c`，数组下标用数字段；任一层缺失返回 `false`。
- `ReadStrings` 支持 `["a"]`（字符串数组）、单字符串、以及数组元素为对象时取 `name`/`title`/`keyword` 字段。
- `BuildDraft`：`FieldMap` 决定取值，`PublishTimePaths` 取出候选值（数字→字符串）放入 `PublishedRaw`，
  `JobURLTemplate` 用 externalId 渲染出 `JobURL`（模板为空则用 raw 里 `jobUrl`/`url`）。
- `Finalize`：调 `parser.ParseSalary` / `parser.ParsePublishTime`，拼 `Location`，生成 `ID`，
  把 nil slice 收敛成 `[]`，缺值字符串收敛成 `""`；`CollectedAt` 为空时用 `ctx.CollectedAt`；`PublishedAtSource` 只能是 `api|detail|missing`。
- `MakeID`：`externalID != "" → platform:externalID`，否则 `platform:` + sha1(fp) 前 16 位。

```go
package dedupe

func Jobs(jobs []model.Job) (out []model.Job, removed int, byID, byFingerprint int)
```

规则：① 按 `ID` 去重；② 同 `platform` 内按 `Fingerprint` 去重；③ 保留 `Completeness` 高者，
相同再比 `SalaryKind` 明确度（range/single > negotiable > unknown），再比 `CollectedAt` 新者；
④ 输出保持首次出现顺序（稳定）。

## 5. G3 · internal/ratelimit 与 internal/httpx

```go
package ratelimit

type Limiter struct{ /* ... */ }
func New(minDelay, maxDelay time.Duration) *Limiter
func (l *Limiter) Wait(ctx context.Context) error   // 全局串行；首次调用立即返回
func (l *Limiter) Stats() (calls int, totalWait time.Duration)
```

```go
package httpx

type Options struct {
    Cookie string
    Headers map[string]string
    Referer string
    UserAgent string
    Timeout time.Duration
    Retries int
    Limiter *ratelimit.Limiter
    Log *slog.Logger
    Platform string
    RespectRobots bool
    AllowDisallowed bool
    RobotsTTL time.Duration
}
type Client struct{ /* ... */ }
func New(opts Options) *Client
type Response struct {
    Status int
    Header http.Header
    Body []byte
    URL string
}
func (c *Client) GetJSON(ctx context.Context, rawURL string) (map[string]any, *Response, error)
func (c *Client) GetText(ctx context.Context, rawURL string) (string, *Response, error)
func (c *Client) GetRaw(ctx context.Context, rawURL string) ([]byte, *Response, error) // 不加 JSON 解析
func (c *Client) CheckRobots(ctx context.Context, rawURL string) (allowed bool, reason string, err error)
func (c *Client) Stats() (requests, retries, blocked int)
```

要求：
1. 每次请求前 `Limiter.Wait`；未注入 Limiter 时不得 panic。
2. robots：拉 `<origin>/robots.txt`，解析 `User-agent: *` 段的 `Disallow`（支持 `*` 通配与 `$` 结尾），
   同 origin 缓存 `RobotsTTL`（默认 1h）。命中且 `RespectRobots && !AllowDisallowed` → `errs.CodeRobotsDisallowed`；
   命中但已授权 → 记 warn 放行。robots 拉取失败（404/网络错）→ 视为未限制，warn 放行。
3. 重试：仅 `errs.IsRetryable` 重试 `Retries` 次，退避 `800ms*2^n + 0~300ms 抖动`。
4. 超时用 `context.WithTimeout`，超时归一 `errs.CodeTimeout`。
5. 非 2xx → `errs.PlatformError`（用 `errs.ClassifyStatus`）。
6. 默认头：桌面 Chrome UA、`Accept-Language: zh-CN,zh;q=0.9`、`Accept: application/json, text/plain, */*`。
7. cookie 日志脱敏（只打印长度与首尾各 2 字符）。

## 6. G4 · 配置与平台适配器

### 6.1 `internal/config`

```go
type Defaults struct {
    Pages, PageSize, MinDelayMS, MaxDelayMS, TimeoutMS, Retries int
    RespectRobots bool
}
type Platform struct {
    ID string `json:"id"`
    Enabled bool `json:"enabled"`
    Priority int `json:"priority"`
    Experimental bool `json:"experimental"`
    Name string `json:"name"`
    Domain string `json:"domain"`
    SearchURL string `json:"searchUrl"`
    JobURLTemplate string `json:"jobUrlTemplate"`
    Referer string `json:"referer"`
    Headers map[string]string `json:"headers"`
    CookieEnv string `json:"cookieEnv"`
    QueryParams map[string]string `json:"queryParams"`
    ListPath string `json:"listPath"`
    HasMorePath string `json:"hasMorePath"`
    CodeField string `json:"codeField"`
    MessageField string `json:"messageField"`
    SuccessCodes []string `json:"successCodes"`
    ErrorCodes map[string]string `json:"errorCodes"`
    FieldMap map[string][]string `json:"fieldMap"`
    PublishTimePaths []string `json:"publishTimePaths"`
    Cities map[string]string `json:"cities"`
    PageSize, MaxPages, MinDelayMS, MaxDelayMS, TimeoutMS, Retries int
    RespectRobots *bool `json:"respectRobots"`
}
type Config struct {
    Version int
    Defaults Defaults
    Platforms map[string]*Platform
}
func Load(path string) (*Config, error) // path=="" → 用 //go:embed platforms.json
func (c *Config) Platform(id string) *Platform // 已填充 Defaults，未配置返回 nil
func (c *Config) Ordered() []*Platform         // 按 Priority 升序
func (p *Platform) DelayRange() (time.Duration, time.Duration)
func (p *Platform) RobotsEnabled() bool
```

`platforms.json` 的顶层结构：`{"version":1,"defaults":{...},"platforms":{"boss":{...}}}`。
`Platform` 的 `ID` 若为空，`Load` 时用 map key 回填。

### 6.2 `internal/platform/httpjson`（可复用引擎）

```go
type Spec struct {
    ID, Name, Domain string
    Experimental bool
    SearchURL string
    QueryParams map[string]string   // 值含占位符 {{query}} {{city}} {{page}} {{pageSize}}
    JobURLTemplate string
    Referer string
    Headers map[string]string
    CookieEnv string
    ListPath string
    HasMorePath string
    CodeField, MessageField string
    SuccessCodes []string
    ErrorCodes map[string]string    // 业务码 -> errs.Code
    FieldMap map[string][]string
    PublishTimePaths []string
    Cities map[string]string
    PageSize, MaxPages int
    // PostMap 可选：平台特有的最终修正钩子
    PostMap func(rt *platform.Runtime, draft *model.JobDraft, raw map[string]any)
}
func SpecFromConfig(p *config.Platform) Spec
func New(spec Spec) *Adapter                 // 实现 platform.Adapter
func (a *Adapter) ID() string
func (a *Adapter) Meta() platform.Meta
func (a *Adapter) Capabilities() platform.Capabilities
func (a *Adapter) NormalizeParams(p model.Params) model.Params
func (a *Adapter) FetchList(ctx context.Context, rt *platform.Runtime, p model.Params) (model.ListResult, error)
func (a *Adapter) MapItem(rt *platform.Runtime, raw map[string]any) (model.JobDraft, error)
func BuildURL(endpoint string, query map[string]string, p model.Params, cityCode string) (string, error)
```

`FetchList` 语义：
1. `NormalizeParams` 已把 `p.City` 转成城市码（存在 `Cities` 时才转）。
2. 渲染 `QueryParams` 占位符 → 拼 URL → `rt.Client.GetJSON`。
3. 校验业务码：`CodeField` 取值不在 `SuccessCodes` → 用 `ErrorCodes` 映射，映射不到则 `errs.ClassifyPayload`，
   再兜底 `errs.CodeUnknown`；**必须返回 error**。
4. 用 `ListPath` 取数组（点路径；为空则顶层数组）；`HasMorePath` 取布尔。
5. 无 cookie 且 `CookieEnv` 已配置且环境变量为空 → 返回 `errs.CodeAuthRequired`（仅 live 模式，见 §6.3）。

`MapItem`：`mapping.BuildDraft(raw, spec)` + 可选 `PostMap`。

### 6.3 `internal/platform/boss` 与 `liepin`

```go
package boss
func New(p *config.Platform) platform.Adapter   // 内部用 httpjson.New(SpecFromConfig(p)) + PostMap
func init()                                     // 不在此注册（需要配置），由 catalog 注入工厂
```

**注册方式（关键设计）**：`platform.Register` 需要实例，而实例需要配置。
因此 `catalog` 提供工厂注册表：

```go
// internal/platform/catalog
type Factory func(p *config.Platform) platform.Adapter
func RegisterFactory(id string, f Factory)
func Build(cfg *config.Config) []platform.Adapter   // 按 cfg.Ordered() 构建并 platform.Register
func New(id string, p *config.Platform) platform.Adapter
```

`boss.RegisterFactory()` / `liepin.RegisterFactory()` 由 catalog 空导入触发。
`pipeline` 通过 `catalog.Build(cfg)` 拿到适配器列表；也可直接接收注入的 `[]platform.Adapter`（便于测试）。

BOSS 特有点（写在 boss 包的 `PostMap` 与 Spec 覆盖里）：
- `jobLabels` 里含经验/学历，若 `Experience`/`Degree` 为空则从标签回填；
- `activeTimeDesc` 为空时用 `bossOnline==true` 回填「在线」；
- 城市名未命中 `Cities` 时原样透传（不报错）。

`liepin` 为骨架，`Experimental: true`，配置里 `enabled: false`，只保证编译与接口正确。

`internal/platform/template/adapter.go`：一个**未被 catalog 引用**的复制即用骨架，注释说明新增平台的 3 步。

## 7. G5 · 编排、存储与 CLI

```go
package pipeline

type Options struct {
    Config *config.Config
    Adapters []platform.Adapter          // 为空时由调用方用 catalog.Build 填充
    Params model.Params
    Log *slog.Logger
    ClientFactory func(p *config.Platform) *httpx.Client // 可空，默认按配置构造
    Progress func(format string, args ...any)            // 可空
    Now func() time.Time                                  // 可空
}
func Collect(ctx context.Context, opt Options) (model.CollectResult, error)
```

流程：遍历适配器 → 查 `Config.Platform(id).Enabled` → 否记 `skipped` → 是则构造 `Runtime` →
逐页 `FetchList` → `MapItem` → `mapping.Finalize` → 收集。单页错误归一入 `stat.Errors`，
遇 `AUTH_REQUIRED / RATE_LIMITED / BLOCKED / ROBOTS_DISALLOWED` **立即停止该平台翻页**（不重试、不中断其他平台）。
最后 `dedupe.Jobs` → 排序（`PublishedAt` 倒序、空值最后；同时间按 `SalaryMonthlyMax` 倒序；再按 `ID`）→ 汇总 `Report`。
`Mode` 为 `sample`/`local` 时不发起网络：`sample` 直接读 `data/jobs.sample.json`；`local` 读 `Params.File` 走对应适配器的 `MapItem`。

> **翻页号语义（易踩坑，务必遵守）**：`model.Params.Pages` 在**调用方**表示「总页数」，
> 在**传给 `FetchList` 的副本**中表示「当前页号（1-based）」。即 pipeline 必须：
> `total := params.Pages（<=0 时取 Config.Defaults.Pages，再以 cfg.MaxPages 截断）`，
> 循环内 `cur := p; cur.Pages = page` 再调用 `FetchList`。
> 适配器只管把 `cur.Pages` 渲染进 `page` 参数，不自行累加页码。

```go
package store

type Store struct{ /* ... */ }
func New(dataDir string) *Store
func (s *Store) LoadJobs() ([]model.Job, string, error)          // source: collected|sample|empty
func (s *Store) SaveJobs(jobs []model.Job) error
func (s *Store) MergeAndSave(incoming []model.Job) (added, total int, err error) // 按 ID 合并，新数据覆盖旧值
func (s *Store) SaveReport(r model.Report) error
func (s *Store) LoadReport() (model.Report, bool)
```

`main.go` 子命令：
```
jobworkspace collect --query=Java --city=北京 --pages=2 --mode=sample|local|live --platforms=boss --file=x.json --cookie=... --out=data/jobs.json --allow-disallowed --json --quiet
jobworkspace serve   --addr=127.0.0.1:8787
jobworkspace platforms
jobworkspace doctor
```
`--mode` 默认 `sample`；`--config` 指定外部配置。退出码：0 成功 / 1 参数或配置错误 / 2 全部平台失败 / 3 部分失败。

## 8. G6 · Web 与前端

```go
package web

type Server struct{ /* ... */ }
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server
func (s *Server) Handler() http.Handler
func (s *Server) Run(addr string) error
```

路由（`http.ServeMux`，Go 1.22 方法模式）：
```
GET  /                    → 内嵌 index.html
GET  /api/health          → {"ok":true,"version":"1.0.0","jobCount":N,"updatedAt":"...","source":"collected|sample|empty","platforms":[...]}
GET  /api/jobs            → {"ok":true,"meta":{"count":N,"updatedAt":"...","source":"...","report":{...}},"jobs":[...]}
GET  /api/platforms       → {"ok":true,"platforms":[{"id","name","enabled","experimental","cities":[...]}]}
POST /api/collect         → {"query","city","pages","mode"} → 调用自身二进制 `collect`（exec.Command(os.Executable())），120s 超时，单飞锁（并发→409）
```
- 静态资源只有 index.html（内嵌），其余一律 404。
- 响应头：`Content-Type: application/json; charset=utf-8`、`Cache-Control: no-store`。
- 仅监听 `127.0.0.1`（由 main 传入默认 addr 保证）。

前端 `internal/web/index.html`：单文件、全内联、零外链、**图标只用内联 SVG（禁 emoji）**、
系统字体栈、`<768px` 单列、按钮 ≥44px、输入框 `font-size:16px`、底部 `env(safe-area-inset-bottom)`。
数据源：`/api/jobs` → 内嵌 `SAMPLE_JOBS`（= `data/jobs.sample.json` 内容，逐字内联）→ `localStorage`。
本地状态 `localStorage["wb_jobws_v1_state"]`，结构：

```js
{ version:1, seeded:true, favorites:["boss:xxx"],
  status:{ "boss:xxx": {stage:"todo|applied|interview|offer|rejected", appliedAt:"", nextFollowUpAt:"", interviewAt:"", note:""} },
  ui:{ keyword:"", city:"", salary:"all", exp:"all", sort:"smart", view:"card" } }
```

四个模块（不得超出）：① 职位库（卡片 + 搜索 + 城市/薪资/经验筛选 + 排序 + 每页12「加载更多」）
② 收藏与投递状态（星标/状态切换/备注）③ 今天要处理（逾期跟进标红 + 一键处理）
④ 采集控制台（关键词/城市/页数 → POST /api/collect，展示各平台状态与错误）。
架构：`refreshAll()` 是唯一刷新入口，渲染函数之间**禁止互调**。

## 9. 完成标准

| 归属 | 标准 |
|---|---|
| G1 | `go test ./internal/parser/` 全绿；§3 表格 10 条全覆盖 |
| G2 | `go test ./internal/dedupe/` 全绿；`Finalize` 对缺字段不 panic |
| G3 | `go test ./internal/ratelimit/` 全绿；`go vet ./internal/httpx/` 通过 |
| G4 | `go build ./internal/config/ ./internal/platform/...` 通过；`platforms.json` 含 boss(启用)/liepin(禁用) |
| G5 | `go build ./...` 通过；`go run . collect --mode=sample --query=Java` 写出 data/jobs.json |
| G6 | `go build ./...` 通过；`go run . serve` 后 `GET /api/jobs` 返回 200 且 jobs 非空 |

**所有子 agent 交付前必须执行**：`gofmt -l <自己的文件>` 输出为空、`go vet ./<自己的包>/`（依赖缺失时改跑 `go build ./<自己的包>/` 并如实报告阻塞点）。
