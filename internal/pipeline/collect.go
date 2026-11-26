// Package pipeline 是采集编排层：平台无关，负责把「参数 + 适配器 + 配置」
// 变成一次可落盘的采集结果。
//
// 设计约定：
//   - 本包**不感知任何具体平台**，只依赖 platform.Adapter 接口，新增平台时本层代码零改动；
//   - 单个平台、单页失败都会被归一进 Report，绝不向上返回 error；
//   - 只有「参数非法 / 配置无法加载」这类全局错误才返回 error；
//   - 采集只有联网一条路径，不读取任何本地示例或抓包文件。
package pipeline

import (
	"context"
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

// Options 是一次采集的全部输入。
//
// 除 Params 外的字段都可留空：Adapters 为空时由 catalog.Build 构建，
// ClientFactory 为空时按配置构造，Log 为空时用 slog.Default，Now 为空时用 time.Now。
type Options struct {
	Config        *config.Config                         // 已加载的配置，必填（否则无法联网）
	Adapters      []platform.Adapter                     // 适配器列表，空则用 catalog.Build(Config)
	Params        model.Params                           // 采集参数
	Log           *slog.Logger                           // 结构化日志
	ClientFactory func(p *config.Platform) *httpx.Client // 自定义 HTTP 客户端构造（测试用）
	Progress      func(format string, args ...any)       // 进度回调，可为空
	Now           func() time.Time                       // 可注入时钟
}

// Collect 执行一次采集并返回岗位与报告。
//
// 返回的 error 仅表示全局性失败（配置缺失、没有可用适配器）；
// 平台级故障一律写入 Report.Platforms[i].Errors 与 Status。
func Collect(ctx context.Context, opt Options) (model.CollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := resolveNow(opt)
	log := resolveLog(opt)
	params := opt.Params

	started := now()
	report := model.Report{
		StartedAt: started.UTC().Format(time.RFC3339),
		Params:    params,
		Platforms: []model.PlatformStat{},
	}

	jobs, stats, err := collectLive(ctx, opt, params, log, now)
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

// ---------- live ----------

// collectLive 按配置逐平台逐页请求真实接口。
func collectLive(ctx context.Context, opt Options, params model.Params, log *slog.Logger, now func() time.Time) ([]model.Job, []model.PlatformStat, error) {
	if opt.Config == nil {
		return nil, nil, fmt.Errorf("采集需要已加载的配置（Config 为空）")
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

// buildRuntime 组装适配器运行时；client 为本次采集使用的 HTTP 客户端。
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
