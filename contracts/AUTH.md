# 登录与后台管理 · 接口契约 v1.1（并行开发唯一真源）

> 在 v1.0.0 基础上新增「登录 + 后台管理」。原则：**不改既有行为**，只做加法。
> 已冻结文件由主控维护：`internal/model`、`internal/errs`、`internal/platform/registry.go`、`internal/web/server.go`、`main.go`。

工程根：仓库根 `job-workspace/`　模块 `jobworkspace`　零第三方依赖（只用标准库）

---

## 0. 硬约束

1. **零第三方依赖**：不引入 `golang.org/x/crypto`、不引入任何 Web 框架。密码哈希自己实现 PBKDF2（`crypto/hmac` + `crypto/sha256`）。
2. **不破坏既有功能**：`GET /`、`GET /api/jobs`、`GET /api/health`、`GET /api/platforms` 保持公开且行为不变。
3. **风格一致**：新增 HTML 复用现有设计令牌（`#0071e3` 主色、`#fbfbfd` 背景、18px 圆角、`cubic-bezier(.4,0,.2,1)` 过渡、系统字体栈），单文件全内联、SVG 图标、零外链、零 emoji。
4. **注释与文档用中文**，导出符号必须有 doc comment。

---

## 1. 文件归属（严禁越界）

| 归属 | 文件 |
|---|---|
| 主控 | `contracts/AUTH.md`、`internal/web/server.go`、`main.go`、`README.md` |
| **H1** | `internal/auth/pbkdf2.go`、`internal/auth/store.go`、`internal/auth/session.go`、`internal/auth/errors.go`、`internal/auth/auth_test.go` |
| **H2** | `internal/store/jobs_crud.go`、`internal/web/auth_http.go`、`internal/web/admin_api.go` |
| **H3** | `internal/web/admin.html`、`internal/web/index.html`（仅限：页脚加「管理」链接、采集 401 提示） |
| **H4** | `internal/version/version.go`、`scripts/*.sh`、`.githooks/pre-push`、`.github/workflows/ci.yml` |

---

## 2. H1 · internal/auth

### 2.1 错误哨兵（errors.go）

```go
var (
    ErrUserExists      = errors.New("用户已存在")
    ErrUserNotFound    = errors.New("用户不存在")
    ErrInvalidPassword = errors.New("密码不正确")
    ErrWeakPassword    = errors.New("密码强度不足")
    ErrLastAdmin       = errors.New("不能删除或降级最后一个管理员")
    ErrInvalidRole     = errors.New("非法角色")
    ErrUserDisabled    = errors.New("账号已禁用")
)
```

### 2.2 密码（pbkdf2.go）

```go
// 参数：PBKDF2-HMAC-SHA256，迭代 210000，盐 16 字节，密钥 32 字节
// 编码格式：pbkdf2-sha256$210000$<saltBase64Std>$<hashBase64Std>
func HashPassword(password string) (string, error)
func VerifyPassword(encoded, password string) bool   // 恒定时间比较；解析失败返回 false，不 panic
func HashPasswordWith(password string, iterations, saltLen, keyLen int) (string, error) // 测试与调参用
func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte  // 内部实现，可导出为 PBKDF2Key 供测试
```

- 密码规则：长度 ≥ 8，≤ 128；不接受纯空白。（只在 `UserStore.Create/SetPassword` 里校验）
- `VerifyPassword` 必须对「格式非法 / 迭代数异常」返回 false 而非 panic；迭代数上限 1_000_000 防 DoS。
- 单测必须包含 **PBKDF2-HMAC-SHA256 标准测试向量**（`PBKDF2Key("password","salt",1,32)` 等 3 组），
  以及「错误密码返回 false」「篡改哈希返回 false」「格式非法返回 false」。

### 2.3 用户存储（store.go）

```go
type Role string
const (
    RoleAdmin  Role = "admin"   // 可增删改
    RoleViewer Role = "viewer"  // 只读
)
func ParseRole(s string) (Role, error)

type User struct {
    Username     string `json:"username"`
    DisplayName  string `json:"displayName"`
    Role         Role   `json:"role"`
    PasswordHash string `json:"passwordHash"`
    CreatedAt    string `json:"createdAt"`
    UpdatedAt    string `json:"updatedAt"`
    Disabled     bool   `json:"disabled"`
}

// PublicUser 是对外（API / 日志）暴露的用户视图，绝不含 PasswordHash。
type PublicUser struct {
    Username    string `json:"username"`
    DisplayName string `json:"displayName"`
    Role        Role   `json:"role"`
    CreatedAt   string `json:"createdAt"`
    UpdatedAt   string `json:"updatedAt"`
    Disabled    bool   `json:"disabled"`
}

type UserStore struct{ /* path, mu, users []User */ }
func NewUserStore(path string) *UserStore
func (s *UserStore) Path() string
func (s *UserStore) Load() error                       // 文件不存在 → 空集合，不报错
func (s *UserStore) Count() int
func (s *UserStore) List() []PublicUser                // 按 username 升序
func (s *UserStore) Get(username string) (User, bool)  // 含哈希，仅供内部校验
func (s *UserStore) Public(username string) (PublicUser, bool)
func (s *UserStore) Create(username, displayName string, role Role, password string) (PublicUser, error)
func (s *UserStore) SetPassword(username, password string) error
func (s *UserStore) Update(username string, displayName *string, role *Role, disabled *bool) (PublicUser, error)
func (s *UserStore) Delete(username string) error
func (s *UserStore) Verify(username, password string) (PublicUser, error)
```

行为要求：
- **原子写**：临时文件 + `os.Rename`，权限 0600，目录 0700。
- `username` 规范化：`strings.TrimSpace` + 小写；长度 3~32；仅允许 `[a-z0-9._-]`。
- `Create` 重复 → `ErrUserExists`；密码不合规 → `ErrWeakPassword`；角色非法 → `ErrInvalidRole`。
- `Delete` / `Update` 若会导致「管理员数量归零」→ `ErrLastAdmin`。
- `Verify`：用户不存在 → 返回 `ErrInvalidPassword`（**不要**暴露「用户不存在」，避免枚举）；`Disabled` → `ErrUserDisabled`。
- `Load` 遇到文件损坏 → 返回带上下文的 error（不要静默清空，防止误覆盖真实用户文件）。
- 并发安全：`sync.RWMutex`。

### 2.4 会话（session.go）

```go
type Session struct {
    ID        string
    Username  string
    Role      Role
    CreatedAt time.Time
    ExpiresAt time.Time
}

type SessionManager struct{ /* mu, sessions map, ttl */ }
func NewSessionManager(ttl time.Duration) *SessionManager
func (m *SessionManager) Create(u PublicUser) (Session, error) // 生成 32 字节随机 ID（base64url）
func (m *SessionManager) Get(id string) (Session, bool)        // 过期即删除并返回 false；剩余 < ttl/2 时滑动续期
func (m *SessionManager) Destroy(id string)
func (m *SessionManager) DestroyUser(username string) int      // 返回被清理的会话数
func (m *SessionManager) Purge()                               // 清理全部过期会话
func (m *SessionManager) Len() int
```

- TTL 默认 12h，由 `web.New` 传入。
- ID 用 `crypto/rand`，32 字节，`base64.RawURLEncoding`。
- 不持久化：进程重启会话失效（单实例、可接受，需在文档说明）。

---

## 3. H2 · 业务增删改 + HTTP 层

### 3.1 `internal/store/jobs_crud.go`

```go
func (s *Store) GetJob(id string) (model.Job, bool, error)
func (s *Store) UpsertJob(j model.Job) (created bool, err error)  // 存在则整体替换，不存在则追加
func (s *Store) DeleteJob(id string) (existed bool, err error)
```
- 必须复用现有原子写与锁，不能与 `MergeAndSave` 产生竞态（同一把 `s.mu`）。
- 不得改变其它岗位的顺序与内容。

### 3.2 `internal/web/auth_http.go`

```go
// 由主控在 server.go 里注册，方法挂在 *Server 上。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request)
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request)
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request)

// 中间件：未登录 → 401 JSON（API）或 302 到 /admin（页面）
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc
func (s *Server) requireRole(role auth.Role, next http.HandlerFunc) http.HandlerFunc

// 内部辅助
func (s *Server) currentSession(r *http.Request) (auth.Session, bool)
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sess auth.Session)
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request)
```

- Cookie：名 `wb_sid`，`HttpOnly`，`SameSite=Lax`，`Path=/`，`MaxAge=TTL秒`，
  `Secure` 当且仅当 `r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"`。
- 登录限流：同一 `(客户端IP, username)` 连续失败 5 次 → 锁 60 秒，返回 429 `{ok:false,error:"TOO_MANY_ATTEMPTS"}`；成功即清零。内存实现即可，按 IP+用户名分桶，定时清理。
- 客户端 IP 取 `X-Forwarded-For` 第一个，否则 `RemoteAddr`。
- 请求体必须是 JSON（`Content-Type: application/json`），否则 415；body 上限 64KB。
- 登录成功响应：`{"ok":true,"user":PublicUser}`；失败：401 `{"ok":false,"error":"INVALID_CREDENTIAL","message":"用户名或密码不正确"}`。
- 登出：销毁会话 + 清 cookie，返回 `{"ok":true}`（未登录也返回 200，幂等）。
- `/api/auth/me`：已登录 `{"ok":true,"authenticated":true,"user":{...}}`；未登录 `{"ok":true,"authenticated":false}`（**不返回 401**，供前端探测）。

### 3.3 `internal/web/admin_api.go`

```go
func (s *Server) handleAdminListJobs(w http.ResponseWriter, r *http.Request)   // GET  /api/admin/jobs?query=&city=
func (s *Server) handleAdminGetJob(w http.ResponseWriter, r *http.Request)     // GET  /api/admin/jobs/{id}
func (s *Server) handleAdminCreateJob(w http.ResponseWriter, r *http.Request)  // POST /api/admin/jobs
func (s *Server) handleAdminUpdateJob(w http.ResponseWriter, r *http.Request)  // PUT  /api/admin/jobs/{id}
func (s *Server) handleAdminDeleteJob(w http.ResponseWriter, r *http.Request)  // DELETE /api/admin/jobs/{id}
```

创建/编辑入参（JSON）：

```jsonc
{
  "jobName": "Java开发工程师",   // 必填，≤80
  "brandName": "某公司",          // 必填，≤80
  "cityName": "北京",             // 必填，≤40
  "areaDistrict": "海淀区",       // 选填，≤40
  "businessDistrict": "中关村",   // 选填，≤40
  "salaryDesc": "20-35K·14薪",    // 选填，≤40；服务端用 parser.ParseSalary 解析出数值
  "experience": "3-5年",          // 选填，≤20
  "degree": "本科",               // 选填，≤20
  "jobLabels": ["后端"],          // 选填，≤10 项，每项 ≤20
  "skills": ["Java"],             // 选填，同上
  "welfareList": ["五险一金"],     // 选填，同上
  "jobUrl": "https://...",        // 选填，必须 http/https，≤500
  "publishedAt": "2026-11-24"     // 选填，YYYY-MM-DD 或 RFC3339
}
```

服务端行为：
- 校验失败 → `422 {"ok":false,"error":"VALIDATION_FAILED","fields":{"jobName":"不能为空"}}`。
- 新建：`Platform="manual"`、`PlatformName="手动录入"`、`Keyword="手动录入"`、`ID="manual:"+12位随机hex`、
  `CollectedAt=now(UTC)`、`SalaryText=parser.FormatSalary(...)`、`PublishedAt` 用 `parser.ParsePublishTime`。
- 编辑：不允许改 `ID` / `Platform`；`CollectedAt` 保留原值。
- 成功：新建 `201 {"ok":true,"job":{...}}`，编辑 `200 {"ok":true,"job":{...}}`，删除 `200 {"ok":true}`。
- 删除不存在 → `404 {"ok":false,"error":"NOT_FOUND"}`。
- 所有响应 `Content-Type: application/json; charset=utf-8` + `Cache-Control: no-store`。

---

## 4. H3 · 前端

### 4.1 `internal/web/admin.html`

单文件、全内联、零外链、SVG 图标、零 emoji，复用现有视觉令牌。两个状态：

**未登录**：居中登录卡片（品牌标 + 「后台管理」+ 用户名/密码 + 登录按钮 + 错误提示行 + 加载态）。
**已登录**：顶栏（品牌 + 当前用户与角色 + 「返回前台」+「退出」）+ 概览数字 + 
岗位表格（职位/公司/城市/薪资/来源/操作）+ 搜索 + 「新增岗位」+ 分页（每页 10）。
新增/编辑用居中浮层表单（字段与 §3.3 一致），删除用二次确认浮层。
校验：前端即时校验（必填、长度、URL 格式）+ 服务端 422 字段级错误回填到对应输入框下方。
反馈：右上角 toast（成功绿 / 失败红），操作后刷新列表。

交互细节：
- 回车提交；`Escape` 关闭浮层；提交中按钮禁用 + 文案「保存中…」。
- 403 → toast「没有权限执行该操作」；401 → 自动切回登录态。

### 4.2 `internal/web/index.html`（仅两处改动）

1. 页脚追加「管理后台」链接（`<a href="/admin">`），样式与页脚既有链接一致。
2. 采集面板提交时若返回 401 → toast「请先登录后台后再采集」并提供「去登录」入口；
   其余逻辑与样式一律不动。

---

## 5. H4 · 版本自动化

### 5.1 `internal/version/version.go`

```go
package version

// Version 是当前版本号，semver 格式，由 scripts/bump-version.sh 自动维护。
// 发布构建会用 -ldflags "-X jobworkspace/internal/version.Version=vX.Y.Z" 覆盖。
var Version = "1.1.0"
```

主控会把 `main.go` 里的 `version` 改为引用本包，H4 不要改 main.go。

### 5.2 脚本

- `scripts/bump-version.sh [major|minor|patch|auto]`
  - `auto`：读 `git log <lastTag>..HEAD`，按 Conventional Commits 判定：
    含 `BREAKING CHANGE:` 或 `type!:` → major；含 `feat:` → minor；其余 → patch；
    无提交或无 tag → 只打印当前版本，不改动。
  - 只改 `internal/version/version.go` 中的版本号，输出新版本到 stdout。
- `scripts/release.sh`
  - 步骤：`gofmt -l` 检查 → `go vet ./...` → `go test ./...` → `go build` → `bump-version.sh auto`
    → `git add -A` → `git commit -m "chore(release): vX.Y.Z"` → `git tag -a vX.Y.Z -m ...` → `git push origin main --follow-tags`
  - `--dry-run` 只打印将要发生的事，不写文件、不提交。
  - 任一步失败立即退出，不留半成品提交。
- `.githooks/pre-push`：推送前跑 `gofmt -l`（必须为空）、`go vet ./...`、`go test ./...`，失败则阻止推送。
- `scripts/install-hooks.sh`：`git config core.hooksPath .githooks`（只改本地配置）。
- `.github/workflows/ci.yml`：`push` 与 `pull_request` 触发，跑 `gofmt`/`vet`/`test`/`build`，
  构建 `linux/amd64` 并上传为 artifact `jobworkspace-ci`。

### 5.3 说明要求

在 `scripts/README.md` 写清：版本判定规则、手动发版流程、为什么不在每次 push 自动提交（避免 CI 与本地互相打架），
以及「自动」的边界——构建/打包/提版号/打标签/推送由一条命令完成，但**是否发版由人决定**。

---

## 6. 验收标准

| 归属 | 标准 |
|---|---|
| H1 | `go test ./internal/auth/` 全绿（含 PBKDF2 向量、最后管理员保护、密码校验、会话过期） |
| H2 | `go build ./...` 通过；`go test ./internal/store/` 若有测试则通过 |
| H3 | 浏览器无控制台报错；401/403/422 三条路径都有可见反馈 |
| H4 | `bash -n` 全部脚本通过；`./scripts/bump-version.sh auto --dry-run` 可运行 |
| 全体 | `gofmt -l .` 为空、`go vet ./...` 干净 |
