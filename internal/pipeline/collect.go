// Package pipeline 是采集编排层：平台无关，负责把「参数 + 适配器 + 配置」
// 变成一次可落盘的采集结果。
//
// 设计约定：
//   - 本包**不感知任何具体平台**，只依赖 platform.Adapter 接口，新增平台时本层代码零改动；
//   - 单个平台、单页失败都会被归一进 Report，绝不向上返回 error；
//   - 只有「参数非法 / 配置无法加载 / 文件读取失败」这类全局错误才返回 error；
//   - sample / local 模式不发起任何网络请求。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"jobworkspace/internal/config"
	"jobworkspace/internal/dedupe"
	"jobworkspace/internal/errs"
	"jobworkspace/internal/httpx"
	"jobworkspace/internal/mapping"
	"jobworkspace/internal/model"
	"jobworkspace/internal/platform"
	"jobworkspace/internal/platform/catalog"
	"jobworkspace/internal/ratelimit"
)

// 采集模式。Params.Mode 为空时按 sample 处理，与 CLI 默认值保持一致。
const (
	// ModeSample 直接读取内置示例数据，不联网。
	ModeSample = "sample"
	// ModeLocal 读取本地保存的原始响应文件，按适配器映射，不联网。
	ModeLocal = "local"
	// ModeLive 按配置逐平台逐页请求真实接口。
	ModeLive = "live"
)

// sampleFile 是 sample 模式默认读取的示例数据路径（相对工作目录）。
const sampleFile = "data/jobs.sample.json"

// Options 是一次采集的全部输入。
//
// 除 Params 外的字段都可留空：Adapters 为空时由 catalog.Build 构建，
// ClientFactory 为空时按配置构造，Log 为空时用 slog.Default，Now 为空时用 time.Now。
type Options struct {
	Config        *config.Config                         // 已加载的配置，sample 模式可为空
	Adapters      []platform.Adapter                     // 适配器列表，空则用 catalog.Build(Config)
	Params        model.Params                           // 采集参数
	Log           *slog.Logger                           // 结构化日志
	ClientFactory func(p *config.Platform) *httpx.Client // 自定义 HTTP 客户端构造（测试用）
	Progress      func(format string, args ...any)       // 进度回调，可为空
	Now           func() time.Time                       // 可注入时钟
}

// Collect 执行一次采集并返回岗位与报告。
//
// 返回的 error 仅表示全局性失败（参数非法、示例文件不可读、本地响应文件不可解析）；
// 平台级故障一律写入 Report.Platforms[i].Errors 与 Status。
func Collect(ctx context.Context, opt Options) (model.CollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := resolveNow(opt)
	log := resolveLog(opt)
	params := opt.Params
	mode := strings.ToLower(strings.TrimSpace(params.Mode))
	if mode == "" {
		mode = ModeSample
	}

	started := now()
	report := model.Report{
		StartedAt: started.UTC().Format(time.RFC3339),
		Params:    params,
		Platforms: []model.PlatformStat{},
	}

	var (
		jobs  []model.Job
		stats []model.PlatformStat
		err   error
	)
	switch mode {
	case ModeSample:
		jobs, stats, err = collectSample(params)
	case ModeLocal:
		jobs, stats, err = collectLocal(opt, params, log, now)
	case ModeLive:
		jobs, stats, err = collectLive(ctx, opt, params, log, now)
	default:
		return model.CollectResult{}, fmt.Errorf("未知采集模式 %q（可选 sample/local/live）", params.Mode)
	}
	if err != nil {
		return model.CollectResult{}, err
	}
	if stats == nil {
		stats = []model.PlatformStat{}
	}

	unique, removed, _, _ := dedupe.Jobs(jobs)
	sortJobs(unique)

	report.Platforms = stats
	report.Totals = buildTotals(stats, unique, removed)
	report.OK = !anyFailed(stats)
	// cookie 与 robots 授权只用于本次请求，绝不写进报告文件。
	report.Params.Cookie = ""
	report.Params.AllowDisallowed = false

	finished := now()
	report.FinishedAt = finished.UTC().Format(time.RFC3339)
	report.DurationMS = finished.Sub(started).Milliseconds()
	if report.DurationMS < 0 {
		report.DurationMS = 0
	}

	if unique == nil {
		unique = []model.Job{}
	}
	return model.CollectResult{Jobs: unique, Report: report}, nil
}

// ---------- sample ----------

// collectSample 读取示例数据文件，只做去重与排序（去重排序在 Collect 内统一完成）。
func collectSample(params model.Params) ([]model.Job, []model.PlatformStat, error) {
	path := sampleFile
	if strings.TrimSpace(params.File) != "" {
		path = strings.TrimSpace(params.File)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取示例数据 %s 失败: %w", path, err)
	}
	var jobs []model.Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, nil, fmt.Errorf("解析示例数据 %s 失败: %w", path, err)
	}
	stat := model.PlatformStat{
		ID:       ModeSample,
		Name:     "示例数据",
		Status:   "ok",
		Pages:    1,
		Fetched:  len(jobs),
		Mapped:   len(jobs),
		Attempts: []model.Attempt{},
		Errors:   []model.ErrItem{},
	}
	return jobs, []model.PlatformStat{stat}, nil
}

// ---------- local ----------

// collectLocal 读取本地原始响应文件，按适配器逐条映射。
func collectLocal(opt Options, params model.Params, log *slog.Logger, now func() time.Time) ([]model.Job, []model.PlatformStat, error) {
	path := strings.TrimSpace(params.File)
	if path == "" {
		return nil, nil, fmt.Errorf("mode=local 必须指定 --file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取本地响应文件 %s 失败: %w", path, err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("解析本地响应文件 %s 失败: %w", path, err)
	}

	adapters := selectAdapters(opt, params.Platforms)
	if len(adapters) == 0 {
		return nil, nil, fmt.Errorf("没有可用的平台适配器（检查 --platforms 或配置）")
	}

	collectedAt := now().UTC().Format(time.RFC3339)
	stats := make([]model.PlatformStat, 0, len(adapters))
	var jobs []model.Job

	for _, a := range adapters {
		cfg := lookupConfig(opt.Config, a.ID())
		listPath := ""
		if cfg != nil {
			listPath = cfg.ListPath
		}
		items := extractItems(root, listPath)

		stat := model.PlatformStat{
			ID:       a.ID(),
			Name:     adapterName(a),
			Status:   "ok",
			Pages:    1,
			Fetched:  len(items),
			Attempts: []model.Attempt{},
			Errors:   []model.ErrItem{},
		}
		if len(items) == 0 {
			stat.Errors = append(stat.Errors, model.ErrItem{
				Code: string(errs.CodeEmpty), Message: "本地文件中未找到岗位数组",
			})
			stat.Status = "failed"
			stats = append(stats, stat)
			continue
		}

		rt := buildRuntime(nil, cfg, log, now)
		spec := adapterSpec(a)
		ctx := mapping.Context{Keyword: params.Query, QueryCity: params.City, CollectedAt: collectedAt}
		for _, rawItem := range items {
			draft, mErr := a.MapItem(rt, rawItem)
			if mErr != nil {
				stat.Dropped++
				code, msg, _ := errs.Info(mErr)
				stat.Errors = append(stat.Errors, model.ErrItem{Code: string(code), Message: msg, Page: 1})
				continue
			}
			jobs = append(jobs, mapping.Finalize(draft, spec, ctx))
			stat.Mapped++
		}
		stat.Status = finalizeStatus(stat)
		stats = append(stats, stat)
	}
	return jobs, stats, nil
}

// ---------- live ----------

// collectLive 按配置逐平台逐页请求真实接口。
func collectLive(ctx context.Context, opt Options, params model.Params, log *slog.Logger, now func() time.Time) ([]model.Job, []model.PlatformStat, error) {
	if opt.Config == nil {
		return nil, nil, fmt.Errorf("mode=live 需要已加载的配置")
	}
	adapters := selectAdapters(opt, params.Platforms)
	if len(adapters) == 0 {
		return nil, nil, fmt.Errorf("没有可用的平台适配器（检查 --platforms 或配置）")
	}

	stats := make([]model.PlatformStat, 0, len(adapters))
	var jobs []model.Job
	collectedAt := now().UTC().Format(time.RFC3339)

	for _, a := range adapters {
		cfg := opt.Config.Platform(a.ID())
		stat := model.PlatformStat{
			ID:       a.ID(),
			Name:     adapterName(a),
			Status:   "ok",
			Attempts: []model.Attempt{},
			Errors:   []model.ErrItem{},
		}
		if cfg == nil || !cfg.Enabled {
			stat.Status = "skipped"
			if cfg == nil {
				stat.Errors = append(stat.Errors, model.ErrItem{
					Code:    string(errs.CodeBadConfig),
					Message: "配置中缺少该平台",
				})
			}
			progress(opt, "[%s] 跳过：未在配置中启用", stat.ID)
			stats = append(stats, stat)
			continue
		}

		rt := buildRuntime(buildClient(opt, cfg, params, log), cfg, log, now)
		p := a.NormalizeParams(params)
		// 页号语义：Params.Pages 在「计划」阶段是总页数，在单次 FetchList 调用中是
		// 当前页号（1-based）。总页数必须用**未归一化**的原始参数算，否则
		// NormalizeParams 把 Pages<1 兜底成 1 后，「0=取配置默认」就永远失效。
		pages := pageCount(opt, params, cfg)
		if p.PageSize <= 0 {
			p.PageSize = cfg.PageSize
		}
		spec := adapterSpec(a)
		mapCtx := mapping.Context{Keyword: p.Query, QueryCity: p.City, CollectedAt: collectedAt}

		progress(opt, "[%s] 开始采集：关键词=%q 城市=%q 计划页数=%d", stat.ID, p.Query, p.City, pages)

		for page := 1; page <= pages; page++ {
			if ctx.Err() != nil {
				break
			}
			pageParams := p
			pageParams.Pages = page
			stat.Pages = page
			pageStart := time.Now()
			res, err := a.FetchList(ctx, rt, pageParams)
			elapsed := time.Since(pageStart).Milliseconds()
			if elapsed < 0 {
				elapsed = 0
			}

			if err != nil {
				code, msg, _ := errs.Info(err)
				stat.Errors = append(stat.Errors, model.ErrItem{Code: string(code), Message: msg, Page: page})
				stat.Attempts = append(stat.Attempts, model.Attempt{
					Name: fmt.Sprintf("list#%d", page), OK: false,
					Code: string(code), Message: msg, MS: elapsed,
				})
				progress(opt, "[%s] 第 %d 页失败：%s %s", stat.ID, page, code, msg)
				// 重试已在 httpx 内完成，此处只决定「是否继续翻页」。
				switch code {
				case errs.CodeAuthRequired, errs.CodeBlocked, errs.CodeRobotsDisallowed:
					stat.Status = "failed"
				case errs.CodeRateLimited:
					if stat.Fetched > 0 {
						stat.Status = "degraded"
					} else {
						stat.Status = "failed"
					}
				default:
					stat.Status = "degraded"
				}
				break
			}

			stat.Attempts = append(stat.Attempts, model.Attempt{
				Name: fmt.Sprintf("list#%d", page), OK: true, MS: elapsed,
			})
			stat.Fetched += len(res.Items)
			mapped := 0
			for _, rawItem := range res.Items {
				draft, mErr := a.MapItem(rt, rawItem)
				if mErr != nil {
					stat.Dropped++
					code, msg, _ := errs.Info(mErr)
					stat.Errors = append(stat.Errors, model.ErrItem{Code: string(code), Message: msg, Page: page})
					continue
				}
				jobs = append(jobs, mapping.Finalize(draft, spec, mapCtx))
				stat.Mapped++
				mapped++
			}
			progress(opt, "[%s] 第 %d 页完成：拉取 %d 条，映射 %d 条", stat.ID, page, len(res.Items), mapped)

			if len(res.Items) == 0 {
				stat.Errors = append(stat.Errors, model.ErrItem{
					Code: string(errs.CodeEmpty), Message: "本页无数据", Page: page,
				})
				break
			}
			if !res.HasMore {
				break
			}
		}

		stat.Status = finalizeStatus(stat)
		progress(opt, "[%s] 采集结束：状态=%s 页数=%d 拉取=%d 映射=%d 丢弃=%d",
			stat.ID, stat.Status, stat.Pages, stat.Fetched, stat.Mapped, stat.Dropped)
		stats = append(stats, stat)
	}
	return jobs, stats, nil
}

// pageCount 计算计划翻页数。
//
// 优先级：用户传入的 raw.Pages（0 = 取配置默认）→ Config.Defaults.Pages →
// 平台 MaxPages → 兜底 2 页；最后统一由 MaxPages 截断（0 表示不限制）。
// 必须传**未经 NormalizeParams** 的原始参数，否则「0 = 默认」会被适配器兜底成 1。
func pageCount(opt Options, raw model.Params, cfg *config.Platform) int {
	total := raw.Pages
	if total <= 0 && opt.Config != nil {
		total = opt.Config.Defaults.Pages
	}
	if total <= 0 && cfg != nil {
		total = cfg.MaxPages
	}
	if total <= 0 {
		total = 2
	}
	if cfg != nil && cfg.MaxPages > 0 && total > cfg.MaxPages {
		total = cfg.MaxPages
	}
	return total
}

// finalizeStatus 在不覆盖「failed/skipped」的前提下，按错误与产出量收敛状态。
func finalizeStatus(stat model.PlatformStat) string {
	if stat.Status == "failed" || stat.Status == "skipped" {
		return stat.Status
	}
	if len(stat.Errors) == 0 && stat.Dropped == 0 {
		return "ok"
	}
	if stat.Mapped == 0 {
		return "failed"
	}
	return "degraded"
}

// ---------- 适配器与运行时 ----------

// selectAdapters 依据 params.Platforms 过滤适配器；为空时返回全部。
func selectAdapters(opt Options, want []string) []platform.Adapter {
	all := opt.Adapters
	if len(all) == 0 && opt.Config != nil {
		all = catalog.Build(opt.Config)
	}
	if len(want) == 0 {
		return all
	}
	set := make(map[string]struct{}, len(want))
	for _, id := range want {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			set[id] = struct{}{}
		}
	}
	out := make([]platform.Adapter, 0, len(all))
	for _, a := range all {
		if _, ok := set[strings.ToLower(a.ID())]; ok {
			out = append(out, a)
		}
	}
	return out
}

// lookupConfig 安全地按 ID 取平台配置（Config 可能为空）。
func lookupConfig(cfg *config.Config, id string) *config.Platform {
	if cfg == nil {
		return nil
	}
	return cfg.Platform(id)
}

// buildClient 构造 HTTP 客户端：优先使用注入的工厂，否则按配置装配限速/重试/robots。
func buildClient(opt Options, cfg *config.Platform, params model.Params, log *slog.Logger) *httpx.Client {
	if opt.ClientFactory != nil {
		return opt.ClientFactory(cfg)
	}
	minDelay, maxDelay := cfg.DelayRange()
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return httpx.New(httpx.Options{
		Cookie:          resolveCookie(cfg, params.Cookie),
		Headers:         cfg.Headers,
		Referer:         cfg.Referer,
		Timeout:         timeout,
		Retries:         cfg.Retries,
		Limiter:         ratelimit.New(minDelay, maxDelay),
		Log:             log,
		Platform:        cfg.ID,
		RespectRobots:   cfg.RobotsEnabled(),
		AllowDisallowed: params.AllowDisallowed,
		RobotsTTL:       time.Hour,
	})
}

// resolveCookie 取 cookie：显式参数优先，其次读平台约定的环境变量。
func resolveCookie(cfg *config.Platform, explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if cfg == nil || strings.TrimSpace(cfg.CookieEnv) == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(cfg.CookieEnv))
}

// buildRuntime 组装适配器运行时；client 可为空（local 模式）。
func buildRuntime(client *httpx.Client, cfg *config.Platform, log *slog.Logger, now func() time.Time) *platform.Runtime {
	rt := &platform.Runtime{Client: client, Config: cfg, Log: log, Now: now}
	if cfg != nil {
		rt.Cities = cfg.Cities
	}
	return rt
}

// adapterSpec 构造映射规格：Finalize 只用到平台标识与展示名。
func adapterSpec(a platform.Adapter) mapping.Spec {
	return mapping.Spec{Platform: a.ID(), PlatformName: adapterName(a)}
}

// adapterName 取平台中文名，Meta 为空时退化为 ID。
func adapterName(a platform.Adapter) string {
	if name := strings.TrimSpace(a.Meta().Name); name != "" {
		return name
	}
	return a.ID()
}

// ---------- 汇总 ----------

// buildTotals 汇总全局统计。
func buildTotals(stats []model.PlatformStat, jobs []model.Job, removed int) model.Totals {
	t := model.Totals{Platforms: len(stats), Unique: len(jobs), Duplicates: removed}
	for _, s := range stats {
		t.Fetched += s.Fetched
		t.Mapped += s.Mapped
		t.Dropped += s.Dropped
	}
	return t
}

// anyFailed 判断是否存在失败平台（skipped 不算失败）。
func anyFailed(stats []model.PlatformStat) bool {
	for _, s := range stats {
		if s.Status == "failed" {
			return true
		}
	}
	return false
}

// sortJobs 排序：发布时间新的在前（空值最后），同时间按薪资上限倒序（nil 最后），
// 再按 ID 升序保证结果稳定。RFC3339 字符串可直接按字典序比较。
func sortJobs(jobs []model.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		a, b := jobs[i], jobs[j]
		aHas, bHas := a.PublishedAt != "", b.PublishedAt != ""
		if aHas != bHas {
			return aHas
		}
		if a.PublishedAt != b.PublishedAt {
			return a.PublishedAt > b.PublishedAt
		}
		switch {
		case a.SalaryMonthlyMax != nil && b.SalaryMonthlyMax != nil:
			if *a.SalaryMonthlyMax != *b.SalaryMonthlyMax {
				return *a.SalaryMonthlyMax > *b.SalaryMonthlyMax
			}
		case a.SalaryMonthlyMax != nil:
			return true
		case b.SalaryMonthlyMax != nil:
			return false
		}
		return a.ID < b.ID
	})
}

// extractItems 按点路径取条目数组，失败时依次回退到常见结构。
func extractItems(root any, listPath string) []map[string]any {
	if m, ok := root.(map[string]any); ok {
		if items := toItems(readPath(m, listPath)); len(items) > 0 {
			return items
		}
		for _, fallback := range []string{"jobList", "zpData.jobList"} {
			if items := toItems(readPath(m, fallback)); len(items) > 0 {
				return items
			}
		}
	}
	return toItems(root)
}

// readPath 是 mapping.ReadPath 的空路径安全包装。
func readPath(m map[string]any, path string) any {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	v, ok := mapping.ReadPath(m, path)
	if !ok {
		return nil
	}
	return v
}

// toItems 把任意已取出的一层结构转成条目数组。
func toItems(v any) []map[string]any {
	switch list := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, it := range list {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return list
	case map[string]any:
		// 单条目也算一条数据。
		return []map[string]any{list}
	}
	return nil
}

// ---------- 基础工具 ----------

// resolveNow 取注入时钟，缺省 time.Now。
func resolveNow(opt Options) func() time.Time {
	if opt.Now != nil {
		return opt.Now
	}
	return time.Now
}

// resolveLog 取注入日志，缺省 slog.Default。
func resolveLog(opt Options) *slog.Logger {
	if opt.Log != nil {
		return opt.Log
	}
	return slog.Default()
}

// progress 在回调非空时输出进度，格式串与参数原样透传。
func progress(opt Options, format string, args ...any) {
	if opt.Progress == nil {
		return
	}
	opt.Progress(format, args...)
}
