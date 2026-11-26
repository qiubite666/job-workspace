// Command jobworkspace 是招聘工作台的单一入口，既是 CLI 也是本地服务。
//
// 五个子命令：
//
//	collect  采集岗位并落盘（sample / local / live 三种模式）
//	serve    启动本地工作台 Web 服务
//	platforms 列出已注册平台与配置状态
//	doctor   体检：Go 版本、数据目录、适配器、cookie 可达性
//	user     后台账号管理（add / list / passwd / delete）
//
// 退出码：0 成功 / 1 参数或配置错误 / 2 全部平台失败 / 3 部分平台失败。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"jobworkspace/internal/auth"
	"jobworkspace/internal/config"
	"jobworkspace/internal/model"
	"jobworkspace/internal/pipeline"
	"jobworkspace/internal/platform"
	"jobworkspace/internal/platform/boss"
	"jobworkspace/internal/platform/catalog"
	"jobworkspace/internal/platform/liepin"
	"jobworkspace/internal/store"
	jobversion "jobworkspace/internal/version"
	"jobworkspace/internal/web"
)

// version 是二进制版本号，供 serve 的 /api/health 与发布产物使用。
// 真源在 internal/version，发布构建通过
// -ldflags "-X jobworkspace/internal/version.Version=<tag>" 覆盖该变量。
var version = jobversion.Version

// defaultAddr 是 serve 的默认监听地址，强制回环地址，避免误暴露到局域网。
const defaultAddr = "127.0.0.1:8787"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 解析子命令并返回退出码，便于测试与统一错误出口。
func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "collect":
		return runCollect(args[1:])
	case "serve":
		return runServe(args[1:])
	case "platforms":
		return runPlatforms(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "user":
		return runUser(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", args[0])
		usage(os.Stderr)
		return 1
	}
}

// usage 打印命令总览。
func usage(w *os.File) {
	fmt.Fprintf(w, `jobworkspace v%s —— 招聘工作台（采集 + 工作台）

用法:
  jobworkspace collect   [flags]   采集岗位并写入 data/jobs.json
  jobworkspace serve     [flags]   启动本地工作台
  jobworkspace platforms           列出已注册平台
  jobworkspace doctor              环境体检
  jobworkspace user      <add|list|passwd|delete>   后台账号管理

collect 参数: --query --city --pages --page-size --platforms --mode --file
              --cookie --allow-disallowed --out --json --quiet --config
serve 参数:   --addr --config
user 参数:    --data --username --name --role --password
`, version)
}

// ---------- 初始化 ----------

// initAdapters 显式注册平台工厂。
//
// 放在函数里而不是包级 init，是为了让注册时机确定：必须先加载配置、
// 注入优先级解析器，再由 catalog.Build 用配置实例化适配器。
func initAdapters() {
	boss.RegisterFactory()
	liepin.RegisterFactory()
}

// loadConfig 加载配置并注入优先级解析器，返回配置与日志。
func loadConfig(path string, quiet bool) (*config.Config, *slog.Logger, error) {
	logger := newLogger(quiet)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, logger, fmt.Errorf("加载配置失败: %w", err)
	}
	platform.SetPriorityResolver(func(id string) int {
		if cfg == nil {
			return 100
		}
		if p := cfg.Platform(id); p != nil {
			return p.Priority
		}
		return 100
	})
	return cfg, logger, nil
}

// newLogger 构造结构化日志：quiet 时只保留 warn 以上，且输出到 stderr。
func newLogger(quiet bool) *slog.Logger {
	level := slog.LevelInfo
	if quiet {
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// ---------- collect ----------

// runCollect 执行采集：解析参数 → pipeline.Collect → 合并落盘 → 打印摘要。
//
// 输出：默认逐平台进度与最终摘要（stderr）；--json 时把 {added,total,report}
// 打到 stdout，供 Web 层直接解析。
func runCollect(args []string) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		query      = fs.String("query", "", "搜索关键词")
		city       = fs.String("city", "北京", "城市名")
		pages      = fs.Int("pages", 0, "采集页数，0 表示取配置默认值")
		pageSize   = fs.Int("page-size", 0, "每页条数，0 表示取配置默认值")
		platformsF = fs.String("platforms", "", "限定平台，逗号分隔，如 boss,liepin")
		mode       = fs.String("mode", pipeline.ModeSample, "采集模式：sample|local|live")
		file       = fs.String("file", "", "mode=local 时的原始响应文件路径")
		cookie     = fs.String("cookie", "", "登录态 cookie（优先级最高，但会留在 shell 历史里，不推荐）")
		cookieFile = fs.String("cookie-file", "", "从文件读取登录态 cookie，推荐用于部署（0600 权限，避免进环境变量）")
		allow      = fs.Bool("allow-disallowed", false, "显式放行 robots.txt 禁止的路径")
		out        = fs.String("out", "data/jobs.json", "岗位数据输出路径")
		asJSON     = fs.Bool("json", false, "以 JSON 输出结果到 stdout")
		quiet      = fs.Bool("quiet", false, "只输出错误")
		configPath = fs.String("config", "", "外部配置文件路径，空则用内置配置")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, logger, err := loadConfig(*configPath, *quiet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	initAdapters()

	modeValue := strings.ToLower(strings.TrimSpace(*mode))
	switch modeValue {
	case pipeline.ModeSample, pipeline.ModeLocal, pipeline.ModeLive:
	default:
		fmt.Fprintf(os.Stderr, "非法的 --mode %q：可选 sample|local|live\n", *mode)
		return 1
	}

	// cookie 取值优先级：--cookie > --cookie-file > 环境变量 BOSS_COOKIE。
	// 部署场景推荐 --cookie-file：凭据不进入 systemd 的 EnvironmentFile（省掉引号与分号解析的坑），
	// 也不会出现在 /proc/<pid>/environ 与 systemctl show 里。
	cookieValue := strings.TrimSpace(*cookie)
	if cookieValue == "" && strings.TrimSpace(*cookieFile) != "" {
		b, err := os.ReadFile(strings.TrimSpace(*cookieFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 cookie 文件失败: %v\n", err)
			return 1
		}
		cookieValue = strings.TrimSpace(string(b))
		if cookieValue == "" {
			fmt.Fprintf(os.Stderr, "cookie 文件为空: %s\n", *cookieFile)
			return 1
		}
	}
	if cookieValue == "" {
		cookieValue = strings.TrimSpace(os.Getenv("BOSS_COOKIE"))
	}
	// --pages 不显式指定时取配置里的 defaults.pages（pipeline 侧还会再兜底一次）。
	pagesValue := *pages
	if pagesValue <= 0 && cfg != nil {
		pagesValue = cfg.Defaults.Pages
	}
	params := model.Params{
		Query:           strings.TrimSpace(*query),
		City:            strings.TrimSpace(*city),
		Pages:           pagesValue,
		PageSize:        *pageSize,
		Platforms:       splitList(*platformsF),
		Mode:            modeValue,
		File:            strings.TrimSpace(*file),
		Cookie:          cookieValue,
		AllowDisallowed: *allow,
	}
	if params.City == "" {
		params.City = "北京"
	}

	opt := pipeline.Options{
		Config: cfg,
		Params: params,
		Log:    logger,
		Now:    time.Now,
	}
	if modeValue == pipeline.ModeLive || modeValue == pipeline.ModeLocal {
		opt.Adapters = catalog.Build(cfg)
	}
	if !*quiet {
		opt.Progress = func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		}
	}

	res, err := pipeline.Collect(context.Background(), opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "采集失败：%v\n", err)
		return 1
	}

	outPath := strings.TrimSpace(*out)
	if outPath == "" {
		outPath = filepath.Join("data", store.JobsFile)
	}
	st := store.New(filepath.Dir(outPath))
	added, total, err := persist(st, outPath, res.Jobs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入岗位数据失败：%v\n", err)
		return 1
	}
	if err := st.SaveReport(res.Report); err != nil {
		logger.Warn("写入采集报告失败", "err", err)
	}

	if *asJSON {
		payload := struct {
			Added  int          `json:"added"`
			Total  int          `json:"total"`
			Report model.Report `json:"report"`
		}{Added: added, Total: total, Report: res.Report}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "输出 JSON 失败：%v\n", err)
			return 1
		}
	} else if !*quiet {
		printSummary(os.Stdout, res.Report, added, total, outPath)
	}

	return exitCode(res.Report)
}

// persist 把岗位写入磁盘：标准 jobs.json 走合并，自定义文件名直接覆盖。
func persist(st *store.Store, outPath string, jobs []model.Job) (added, total int, err error) {
	if filepath.Base(outPath) == store.JobsFile {
		return st.MergeAndSave(jobs)
	}
	if err := writeJobsFile(outPath, jobs); err != nil {
		return 0, 0, err
	}
	return len(jobs), len(jobs), nil
}

// writeJobsFile 覆盖写入任意路径（自定义 --out 使用），同样保证 JSON 可读性。
func writeJobsFile(path string, jobs []model.Job) error {
	if jobs == nil {
		jobs = []model.Job{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// printSummary 打印各平台状态与全局统计。
func printSummary(w *os.File, r model.Report, added, total int, outPath string) {
	fmt.Fprintf(w, "采集完成：%s → %s（耗时 %dms）\n", r.StartedAt, r.FinishedAt, r.DurationMS)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "平台\t状态\t页数\t拉取\t映射\t丢弃")
	for _, s := range r.Platforms {
		fmt.Fprintf(tw, "%s(%s)\t%s\t%d\t%d\t%d\t%d\n", s.Name, s.ID, s.Status, s.Pages, s.Fetched, s.Mapped, s.Dropped)
	}
	tw.Flush()

	fmt.Fprintf(w, "合计：平台 %d / 拉取 %d / 映射 %d / 丢弃 %d / 唯一 %d / 重复 %d\n",
		r.Totals.Platforms, r.Totals.Fetched, r.Totals.Mapped, r.Totals.Dropped, r.Totals.Unique, r.Totals.Duplicates)
	fmt.Fprintf(w, "写入：新增 %d 条，共 %d 条 → %s\n", added, total, outPath)

	for _, s := range r.Platforms {
		for _, e := range s.Errors {
			fmt.Fprintf(w, "  ! %s 第%d页 %s: %s\n", s.ID, e.Page, e.Code, e.Message)
		}
	}
}

// exitCode 把报告映射为退出码：2 全失败 / 3 部分失败 / 0 正常。
func exitCode(r model.Report) int {
	failed, considered := 0, 0
	for _, s := range r.Platforms {
		if s.Status == "skipped" {
			continue
		}
		considered++
		if s.Status == "failed" {
			failed++
		}
	}
	switch {
	case failed == 0:
		return 0
	case considered > 0 && failed == considered:
		return 2
	default:
		return 3
	}
}

// ---------- serve ----------

// runServe 启动本地工作台，阻塞直到进程退出。
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	addr := fs.String("addr", defaultAddr, "监听地址，建议保持 127.0.0.1")
	configPath := fs.String("config", "", "外部配置文件路径，空则用内置配置")
	dataDir := fs.String("data", "data", "数据目录（岗位、后台账号、采集报告都落在这里）")
	cookieFile := fs.String("cookie-file", "", "登录态 cookie 文件路径；后台触发的采集会读取它")
	quiet := fs.Bool("quiet", false, "只输出错误")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, logger, err := loadConfig(*configPath, *quiet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	initAdapters()
	catalog.Build(cfg)

	srv := web.New(cfg, store.New(*dataDir), logger)
	// 采集子进程需要同一份凭据来源；这里只传路径，cookie 值不进命令行。
	srv.SetCookieFile(strings.TrimSpace(*cookieFile))
	if err := srv.Run(*addr); err != nil {
		fmt.Fprintf(os.Stderr, "服务启动失败：%v\n", err)
		return 1
	}
	return 0
}

// ---------- platforms ----------

// runPlatforms 打印平台清单：ID、名称、启用、实验性、优先级。
func runPlatforms(args []string) int {
	fs := flag.NewFlagSet("platforms", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "外部配置文件路径，空则用内置配置")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, _, err := loadConfig(*configPath, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	initAdapters()
	catalog.Build(cfg)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\t名称\t启用\t实验性\t优先级")
	seen := make(map[string]bool, len(cfg.Platforms))
	for _, a := range platform.All() {
		id := a.ID()
		seen[id] = true
		name, enabled, experimental, priority := a.Meta().Name, false, a.Meta().Experimental, 100
		if p := cfg.Platform(id); p != nil {
			name = firstNonEmpty(p.Name, name, id)
			enabled, experimental, priority = p.Enabled, p.Experimental, p.Priority
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", id, name, yesNo(enabled), yesNo(experimental), priority)
	}
	// 配置里声明了但没有适配器的平台也需要暴露出来。
	for _, p := range cfg.Ordered() {
		if seen[p.ID] {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t(缺少适配器)\n",
			p.ID, firstNonEmpty(p.Name, p.ID), yesNo(p.Enabled), yesNo(p.Experimental), p.Priority)
	}
	tw.Flush()
	return 0
}

// ---------- doctor ----------

// check 是一条体检结果。
type check struct {
	level  string // ok | warn | fail
	title  string
	detail string
}

// runDoctor 体检环境：Go 版本、数据目录、适配器、cookie 存在性、数据来源。
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "外部配置文件路径，空则用内置配置")
	dataDir := fs.String("data", "data", "数据目录")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, _, err := loadConfig(*configPath, true)
	if err != nil {
		fmt.Printf("[fail] 配置加载\t%v\n", err)
		return 1
	}
	initAdapters()
	catalog.Build(cfg)

	checks := []check{
		checkGoVersion(),
		checkDataDir(*dataDir),
	}
	checks = append(checks, checkAdapters(cfg)...)
	checks = append(checks, checkCookies(cfg)...)
	checks = append(checks, checkDataSource(*dataDir))

	fails := 0
	for _, c := range checks {
		fmt.Printf("[%s] %s\t%s\n", c.level, c.title, c.detail)
		if c.level == "fail" {
			fails++
		}
	}
	fmt.Printf("%d 项检查，%d 项失败\n", len(checks), fails)
	if fails > 0 {
		return 1
	}
	return 0
}

// checkGoVersion 要求 Go 1.22+（http.ServeMux 方法模式与 slog 依赖）。
func checkGoVersion() check {
	v := runtime.Version()
	minor := 0
	if s := strings.TrimPrefix(v, "go1."); s != v {
		if i := strings.IndexAny(s, ".-"); i >= 0 {
			s = s[:i]
		}
		minor, _ = strconv.Atoi(s)
	}
	if minor >= 22 {
		return check{"ok", "Go 版本", fmt.Sprintf("%s（需要 1.22+）", v)}
	}
	return check{"fail", "Go 版本", fmt.Sprintf("%s 过低，需要 1.22+", v)}
}

// checkDataDir 验证数据目录可创建、可写入。
func checkDataDir(dir string) check {
	if strings.TrimSpace(dir) == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return check{"fail", "数据目录", fmt.Sprintf("%s 无法创建: %v", dir, err)}
	}
	f, err := os.CreateTemp(dir, ".doctor*")
	if err != nil {
		return check{"fail", "数据目录", fmt.Sprintf("%s 不可写: %v", dir, err)}
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return check{"ok", "数据目录", dir + " 可写"}
}

// checkAdapters 校验配置里的平台是否都有适配器实现。
func checkAdapters(cfg *config.Config) []check {
	out := make([]check, 0, len(cfg.Platforms))
	for _, p := range cfg.Ordered() {
		if _, ok := platform.Get(p.ID); ok {
			out = append(out, check{"ok", "平台 " + p.ID, firstNonEmpty(p.Name, p.ID) + " 适配器已注册"})
			continue
		}
		level := "warn"
		if p.Enabled {
			level = "fail"
		}
		out = append(out, check{level, "平台 " + p.ID, "缺少适配器实现"})
	}
	return out
}

// checkCookies 只报告 cookie 是否存在，绝不打印其值。
func checkCookies(cfg *config.Config) []check {
	out := make([]check, 0, len(cfg.Platforms))
	for _, p := range cfg.Ordered() {
		env := strings.TrimSpace(p.CookieEnv)
		if env == "" || !p.Enabled {
			continue
		}
		if strings.TrimSpace(os.Getenv(env)) != "" {
			out = append(out, check{"ok", "Cookie " + p.ID, env + " 已设置"})
			continue
		}
		out = append(out, check{"warn", "Cookie " + p.ID, env + " 未设置，live 采集会返回 AUTH_REQUIRED"})
	}
	return out
}

// checkDataSource 报告当前数据来源，仅作提示。
func checkDataSource(dataDir string) check {
	st := store.New(dataDir)
	jobs, source, err := st.LoadJobs()
	if err != nil {
		return check{"warn", "数据来源", err.Error()}
	}
	switch source {
	case store.SourceCollected:
		return check{"ok", "数据来源", fmt.Sprintf("已采集 %d 条（%s/%s）", len(jobs), dataDir, store.JobsFile)}
	case store.SourceSample:
		return check{"ok", "数据来源", fmt.Sprintf("示例数据 %d 条（请先跑 collect --mode=live）", len(jobs))}
	default:
		return check{"warn", "数据来源", "暂无数据，请先执行 jobworkspace collect"}
	}
}

// ---------- 工具 ----------

// splitList 按逗号切分并去空、转小写。
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// yesNo 把布尔渲染成中文。
func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// runUser 实现后台账号管理：add / list / passwd / delete。
//
// 账号落在 <data>/users.json，必须与 serve 使用同一个数据目录（默认都是 data）。
// add 不指定 --password 时会生成随机密码并只打印一次，避免明文进 shell 历史。
func runUser(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: jobworkspace user <add|list|passwd|delete> [--data data] [--username xxx]")
		return 1
	}
	sub := args[0]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	dataDir := fs.String("data", "data", "数据目录，需与 serve 一致")
	username := fs.String("username", "", "用户名（3~32 位小写字母/数字/._-）")
	display := fs.String("name", "", "显示名，默认与用户名相同")
	role := fs.String("role", "admin", "角色：admin（可增删改）| viewer（只读）")
	password := fs.String("password", "", "密码，留空则自动生成 16 位随机密码")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	us := auth.NewUserStore(filepath.Join(*dataDir, "users.json"))
	if err := us.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "加载用户文件失败: %v\n", err)
		return 1
	}

	switch sub {
	case "add":
		if strings.TrimSpace(*username) == "" {
			fmt.Fprintln(os.Stderr, "缺少 --username")
			return 1
		}
		r, err := auth.ParseRole(*role)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		pw := *password
		generated := false
		if pw == "" {
			pw = randomPassword()
			generated = true
		}
		name := *display
		if name == "" {
			name = *username
		}
		u, err := us.Create(*username, name, r, pw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建失败: %v\n", err)
			return 1
		}
		fmt.Printf("已创建用户 %s（角色 %s，文件 %s）\n", u.Username, u.Role, us.Path())
		if generated {
			fmt.Printf("初始密码: %s\n（仅显示这一次，请立即保存；可用 user passwd 重置）\n", pw)
		}
		return 0

	case "list":
		users := us.List()
		if len(users) == 0 {
			fmt.Println("尚无后台账号，请先执行: jobworkspace user add --username admin")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "用户名\t显示名\t角色\t状态\t更新时间")
		for _, u := range users {
			state := "启用"
			if u.Disabled {
				state = "已禁用"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Username, u.DisplayName, u.Role, state, u.UpdatedAt)
		}
		return flushTab(tw)

	case "passwd":
		if strings.TrimSpace(*username) == "" {
			fmt.Fprintln(os.Stderr, "缺少 --username")
			return 1
		}
		pw := *password
		generated := false
		if pw == "" {
			pw = randomPassword()
			generated = true
		}
		if err := us.SetPassword(*username, pw); err != nil {
			fmt.Fprintf(os.Stderr, "重置失败: %v\n", err)
			return 1
		}
		fmt.Printf("已重置 %s 的密码\n", *username)
		if generated {
			fmt.Printf("新密码: %s\n（仅显示这一次）\n", pw)
		}
		fmt.Println("提示：已登录的浏览器会话仍然有效，直至过期或服务重启；如需立即失效请重启服务。")
		return 0

	case "delete":
		if strings.TrimSpace(*username) == "" {
			fmt.Fprintln(os.Stderr, "缺少 --username")
			return 1
		}
		if err := us.Delete(*username); err != nil {
			fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
			return 1
		}
		fmt.Printf("已删除用户 %s\n", *username)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "未知的 user 子命令 %q（可选 add|list|passwd|delete）\n", sub)
		return 1
	}
}

// flushTab 把表格缓冲刷出并返回退出码，避免每个分支重复处理 err。
func flushTab(tw *tabwriter.Writer) int {
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "输出失败: %v\n", err)
		return 1
	}
	return 0
}

// randomPassword 生成 16 位 URL 安全随机密码，用于账号初始化。
func randomPassword() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于系统级异常，退回时间戳不足以作为长期方案，
		// 这里显式提示而不是静默降级。
		fmt.Fprintf(os.Stderr, "生成随机密码失败: %v\n", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
