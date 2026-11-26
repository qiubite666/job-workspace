// Package web 提供本地 Web 服务：内嵌单文件前端页面、只读 JSON API，
// 以及通过重新执行自身二进制触发的采集接口。
//
// 设计约束（见 contracts/CONTRACT.md 第 8 节）：
//   - 只服务一个内嵌 index.html，其余静态路径一律 404；
//   - 所有 JSON 响应都带 no-store，不做 CORS（同源）；
//   - 采集接口只允许单飞，并发请求直接返回 409。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"jobworkspace/internal/auth"
	"jobworkspace/internal/config"
	"jobworkspace/internal/model"
	"jobworkspace/internal/store"
	"jobworkspace/internal/userdata"

	"log/slog"
)

// version 是 /api/health 上报的服务版本号。
const version = "1.0.0"

// collectTimeout 是 /api/collect 触发子进程的硬超时。
const collectTimeout = 120 * time.Second

// stderrTailLimit 是失败时回传的子进程 stderr 最大字符数。
const stderrTailLimit = 2000

// indexHTML 是编译期嵌入的单文件前端，CSS/JS/图标全部内联。
//
//go:embed index.html
var indexHTML []byte

//go:embed admin.html
var adminHTML []byte

// collectMu 保证同一时刻只有一个采集子进程在运行。
//
// 使用 TryLock 而非 Lock：并发请求应当立刻拿到 409，而不是排队等待。
var collectMu sync.Mutex

// Server 是本地 Web 服务实例，持有配置、存储、认证与日志依赖。
type Server struct {
	cfg     *config.Config
	st      *store.Store
	log     *slog.Logger
	handler http.Handler

	// users 与 sessions 是 v1.1 新增的认证依赖：
	// 用户落盘在 <data>/users.json，会话仅存内存（进程重启即失效）。
	users    *auth.UserStore
	sessions *auth.SessionManager

	// cookieFile 是登录态 cookie 的文件路径（可为空）。
	// 只存路径不存值：采集子进程自己读文件，避免凭据出现在命令行参数里。
	cookieFile string

	// userState 存放每个账号的收藏 / 投递状态 / 备注，路径为 <data>/userstate。
	userState *userdata.Store
}

// SetCookieFile 设置采集子进程使用的 cookie 文件路径。
//
// 传入空字符串表示不启用；文件不存在或不可读时，采集会按「无登录态」处理并返回 AUTH_REQUIRED。
func (s *Server) SetCookieFile(path string) {
	s.cookieFile = strings.TrimSpace(path)
}

// sessionTTL 是登录会话的有效期；剩余不足一半时会自动续期。
const sessionTTL = 12 * time.Hour

// New 构造 Web 服务并完成路由注册。
//
// cfg 允许为 nil（例如测试场景），此时平台相关接口返回空列表而不是 panic。
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		cfg:      cfg,
		st:       st,
		log:      log,
		sessions: auth.NewSessionManager(sessionTTL),
	}

	// 认证存储跟随数据目录，缺文件视为「尚未初始化」，不阻断服务启动。
	if st != nil {
		s.users = auth.NewUserStore(filepath.Join(st.Dir(), "users.json"))
		if err := s.users.Load(); err != nil {
			log.Error("加载用户文件失败，后台登录将不可用", "err", err, "path", s.users.Path())
		} else if s.users.Count() == 0 {
			log.Warn("尚未创建任何后台账号，请先执行 `jobworkspace user add --username admin`")
		}

		// 每个账号的收藏 / 投递状态 / 备注，落在 <data>/userstate/<username>.json。
		s.userState = userdata.NewStore(filepath.Join(st.Dir(), "userstate"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /admin", s.handleAdminPage)
	// 健康检查与平台清单是静态信息，保持公开：前者供监控探活，
	// 后者是内置配置（不含任何用户数据），登录前就要用来渲染采集面板。
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/platforms", s.handlePlatforms)

	// 岗位数据与采集自 v2 起要求登录。
	mux.HandleFunc("GET /api/jobs", s.requireAuth(s.handleJobs))
	mux.HandleFunc("POST /api/collect", s.requireAuth(s.handleCollect))

	// 当前登录用户自己的收藏 / 投递状态 / 备注，按账号隔离。
	mux.HandleFunc("GET /api/me/state", s.requireAuth(s.handleGetMyState))
	mux.HandleFunc("PUT /api/me/state", s.requireAuth(s.handlePutMyState))

	// 认证
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)

	// 后台管理：读接口 viewer 亦可，写接口仅 admin。
	mux.HandleFunc("GET /api/admin/jobs", s.requireAuth(s.handleAdminListJobs))
	mux.HandleFunc("POST /api/admin/jobs", s.requireRole(auth.RoleAdmin, s.handleAdminCreateJob))
	mux.HandleFunc("GET /api/admin/jobs/{id}", s.requireAuth(s.handleAdminGetJob))
	mux.HandleFunc("PUT /api/admin/jobs/{id}", s.requireRole(auth.RoleAdmin, s.handleAdminUpdateJob))
	mux.HandleFunc("DELETE /api/admin/jobs/{id}", s.requireRole(auth.RoleAdmin, s.handleAdminDeleteJob))

	s.handler = withLogging(log, mux)
	return s
}

// handleAdminPage 返回内嵌的后台管理单页。
//
// 页面本身公开可取（内部只有登录卡片），真正的数据保护在 API 层；
// 这样未登录用户也能看到登录框，而不是被 302 打断。
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "页面不存在")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminHTML)
}

// Handler 返回已挂好路由与日志中间件的 http.Handler。
func (s *Server) Handler() http.Handler {
	if s.handler != nil {
		return s.handler
	}
	return http.NotFoundHandler()
}

// Run 在 addr 上阻塞式启动 HTTP 服务，直到进程被终止或监听失败。
//
// addr 通常由 main 传入 127.0.0.1:8787，本方法不负责绑定外网地址。
func (s *Server) Run(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.log.Info("Web 服务已启动", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("启动 Web 服务失败（addr=%s）: %w", addr, err)
	}
	return nil
}

// handleIndex 返回内嵌的单文件前端。
//
// 因为 "GET /" 是子树模式，必须显式把非根路径判成 404，否则任意路径都会返回首页。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "页面不存在")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(indexHTML); err != nil {
		s.log.Warn("写入首页响应失败", "err", err)
	}
}

// platformBrief 是 /api/health 与 /api/platforms 共用的平台摘要。
type platformBrief struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Experimental bool     `json:"experimental"`
	Cities       []string `json:"cities,omitempty"`
}

// handleHealth 汇报服务存活状态、岗位总数、数据来源与平台清单。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jobs, source, err := s.loadJobs()
	if err != nil {
		s.log.Warn("健康检查读取岗位失败", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"version":   version,
		"jobCount":  len(jobs),
		"updatedAt": s.updatedAt(jobs),
		"source":    source,
		"platforms": s.platforms(false),
	})
}

// jobsMeta 是 /api/jobs 的元信息。
type jobsMeta struct {
	Count     int          `json:"count"`
	UpdatedAt string       `json:"updatedAt"`
	Source    string       `json:"source"`
	Report    model.Report `json:"report"`
}

// handleJobs 返回岗位列表，并支持 query/city 的轻量服务端过滤。
//
// 前端仍会自行做完整筛选与排序，这里的过滤只是减少首次传输量。
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, source, err := s.loadJobs()
	if err != nil {
		s.log.Warn("读取岗位失败", "err", err)
		writeError(w, http.StatusInternalServerError, "STORE_READ_FAILED", "读取岗位数据失败")
		return
	}

	keyword := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	city := strings.TrimSpace(r.URL.Query().Get("city"))

	out := make([]model.Job, 0, len(jobs))
	for _, j := range jobs {
		if keyword != "" && !jobMatchKeyword(j, keyword) {
			continue
		}
		if city != "" && !jobMatchCity(j, city) {
			continue
		}
		out = append(out, j)
	}

	report := model.Report{}
	if s.st != nil {
		if rp, ok := s.st.LoadReport(); ok {
			report = rp
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"meta": jobsMeta{
			Count:     len(out),
			UpdatedAt: s.updatedAt(jobs),
			Source:    source,
			Report:    report,
		},
		"jobs": out,
	})
}

// handlePlatforms 返回全部平台（含未启用平台）及其支持的城市列表。
func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"platforms": s.platforms(true),
	})
}

// collectRequest 是 POST /api/collect 的请求体。
type collectRequest struct {
	Query           string `json:"query"`
	City            string `json:"city"`
	Pages           int    `json:"pages"`
	AllowDisallowed bool   `json:"allowDisallowed"`
}

// handleCollect 同步执行一次采集：重新执行自身二进制的 collect 子命令。
//
// 并发调用直接返回 409；超时或子进程失败返回 500 并附带 stderr 尾部。
func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	var req collectRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON："+err.Error())
		return
	}

	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "关键词不能为空")
		return
	}
	if req.Pages <= 0 {
		req.Pages = 2
	}
	if req.Pages > 10 {
		req.Pages = 10
	}

	if !collectMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":      false,
			"error":   "BUSY",
			"message": "已有采集任务在进行",
		})
		return
	}
	defer collectMu.Unlock()

	before, _, _ := s.loadJobs()

	executable, err := os.Executable()
	if err != nil {
		s.log.Error("无法定位自身可执行文件", "err", err)
		writeError(w, http.StatusInternalServerError, "COLLECT_FAILED", "无法定位自身可执行文件："+err.Error())
		return
	}

	args := []string{
		"collect",
		"--query=" + req.Query,
		"--city=" + req.City,
		"--pages=" + strconv.Itoa(req.Pages),
		"--json",
		"--quiet",
	}
	if req.AllowDisallowed {
		args = append(args, "--allow-disallowed")
	}
	if s.cookieFile != "" {
		args = append(args, "--cookie-file="+s.cookieFile)
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectTimeout)
	defer cancel()

	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}

	s.log.Info("开始采集", "query", req.Query, "city", req.City, "pages", req.Pages, "allowDisallowed", req.AllowDisallowed)
	runErr := cmd.Run()
	detail := tailRunes(stderr.String(), stderrTailLimit)
	// 子进程带 --json --quiet 运行时，错误信息在 stdout 的 report 里，stderr 是空的。
	// 失败时把 stdout 也作为 detail 兜底，否则前端只能看到无信息量的 "exit status 2"。
	if detail == "" {
		detail = tailRunes(stdout.String(), stderrTailLimit)
	}

	if runErr != nil {
		code := "COLLECT_FAILED"
		msg := "采集失败：" + runErr.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "TIMEOUT"
			msg = "采集超时（超过 " + collectTimeout.String() + "）"
		}
		s.log.Error("采集失败", "err", runErr, "code", code, "stderr", detail)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":      false,
			"error":   code,
			"message": msg,
			"detail":  detail,
		})
		return
	}

	parsed, hasJSON := parseCollectOutput(stdout.String())
	after, source, loadErr := s.loadJobs()
	if loadErr != nil {
		s.log.Warn("采集后读取岗位失败", "err", loadErr)
	}

	added := len(after) - len(before)
	if added < 0 {
		added = 0
	}
	if hasJSON && parsed.Added != nil && *parsed.Added > added {
		added = *parsed.Added
	}

	report := model.Report{}
	if hasJSON && parsed.Report != nil {
		report = *parsed.Report
	} else if s.st != nil {
		if rp, ok := s.st.LoadReport(); ok {
			report = rp
		}
	}
	if !hasJSON {
		s.log.Warn("采集子进程未输出可解析的 JSON，已回退到存储统计", "stdoutHead", tailRunes(stdout.String(), 400))
	}

	resp := map[string]any{
		"ok":      true,
		"added":   added,
		"total":   len(after),
		"source":  source,
		"report":  report,
		"updated": s.updatedAt(after),
	}
	if detail != "" {
		resp["stderr"] = detail
	}
	writeJSON(w, http.StatusOK, resp)
}

// collectCLIOut 是采集子进程 --json 输出的宽容结构：字段缺失即视为未知。
type collectCLIOut struct {
	OK     *bool         `json:"ok"`
	Added  *int          `json:"added"`
	Total  *int          `json:"total"`
	Count  *int          `json:"count"`
	Report *model.Report `json:"report"`
}

// parseCollectOutput 尽力从子进程 stdout 中解析出 JSON 结果。
//
// 子进程可能混入进度日志（即便加了 --quiet），因此先整体解析，失败后从末尾
// 逐行向前寻找第一个以 '{' 开头的行，再尝试解析其后的全部内容。
func parseCollectOutput(out string) (collectCLIOut, bool) {
	var res collectCLIOut
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return res, false
	}
	if err := json.Unmarshal([]byte(trimmed), &res); err == nil {
		return res, true
	}
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "{") {
			continue
		}
		candidate := strings.Join(lines[i:], "\n")
		if err := json.Unmarshal([]byte(candidate), &res); err == nil {
			return res, true
		}
	}
	return res, false
}

// loadJobs 读取岗位列表；存储不可用时返回空列表而不是错误中断。
func (s *Server) loadJobs() ([]model.Job, string, error) {
	if s.st == nil {
		return []model.Job{}, "empty", nil
	}
	jobs, source, err := s.st.LoadJobs()
	if err != nil {
		return []model.Job{}, "empty", fmt.Errorf("读取岗位列表失败: %w", err)
	}
	if jobs == nil {
		jobs = []model.Job{}
	}
	return jobs, source, nil
}

// platforms 汇总平台信息；withCities 为 true 时附带排序后的城市名列表。
func (s *Server) platforms(withCities bool) []platformBrief {
	out := make([]platformBrief, 0, 4)
	if s.cfg == nil {
		return out
	}
	for _, p := range s.cfg.Ordered() {
		if p == nil {
			continue
		}
		brief := platformBrief{
			ID:           p.ID,
			Name:         p.Name,
			Enabled:      p.Enabled,
			Experimental: p.Experimental,
		}
		if brief.Name == "" {
			brief.Name = p.ID
		}
		if withCities {
			cities := make([]string, 0, len(p.Cities))
			for name := range p.Cities {
				cities = append(cities, name)
			}
			sort.Strings(cities)
			brief.Cities = cities
		}
		out = append(out, brief)
	}
	return out
}

// updatedAt 取岗位采集时间的最大值；全部缺失时回退到采集报告完成时间。
func (s *Server) updatedAt(jobs []model.Job) string {
	latest := ""
	var latestTime time.Time
	for _, j := range jobs {
		if j.CollectedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, j.CollectedAt)
		if err != nil {
			continue
		}
		if latest == "" || t.After(latestTime) {
			latest, latestTime = j.CollectedAt, t
		}
	}
	if latest != "" {
		return latest
	}
	if s.st != nil {
		if rp, ok := s.st.LoadReport(); ok {
			return rp.FinishedAt
		}
	}
	return ""
}

// jobMatchKeyword 判断岗位是否命中关键词（职位名/公司/平台/技能）。
func jobMatchKeyword(j model.Job, keywordLower string) bool {
	if strings.Contains(strings.ToLower(j.JobName), keywordLower) ||
		strings.Contains(strings.ToLower(j.BrandName), keywordLower) ||
		strings.Contains(strings.ToLower(j.PlatformName), keywordLower) {
		return true
	}
	for _, sk := range j.Skills {
		if strings.Contains(strings.ToLower(sk), keywordLower) {
			return true
		}
	}
	return false
}

// jobMatchCity 判断岗位是否命中城市（城市名或组合地点包含）。
func jobMatchCity(j model.Job, city string) bool {
	if j.CityName == city {
		return true
	}
	return strings.Contains(j.Location, city) || strings.Contains(j.QueryCity, city)
}

// writeJSON 以 UTF-8 JSON 写出响应，统一附带 no-store。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	buf, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"ENCODE_FAILED","message":"响应编码失败"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		return
	}
}

// writeError 输出统一的错误 JSON：{"ok":false,"error":code,"message":msg}。
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"ok":      false,
		"error":   code,
		"message": message,
	})
}

// tailRunes 截取字符串末尾至多 n 个字符，避免按字节切断多字节汉字。
func tailRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[len(rs)-n:])
}

// statusRecorder 记录实际写出的状态码，供日志中间件使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// WriteHeader 记录状态码后透传给底层 ResponseWriter。
func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write 在未显式调用 WriteHeader 时按 200 记账。
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap 支持 http.ResponseController 访问底层连接能力。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withLogging 包装一层请求日志：方法、路径、状态码、耗时。
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"took", time.Since(start).Milliseconds(),
		)
	})
}
