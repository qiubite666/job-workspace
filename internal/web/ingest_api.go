package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"jobworkspace/internal/config"
	"jobworkspace/internal/mapping"
	"jobworkspace/internal/model"
)

// ingestBodyLimit 限制上报体积：单页 15~30 条岗位的原始 JSON 通常在 100KB 量级，
// 4MB 足够容纳多页合并，同时避免被当成上传接口刷盘。
const ingestBodyLimit = 4 << 20

// ingestRequest 是浏览器扩展上报的单批数据。
//
// raw 直接携带平台原始响应（BOSS 的 zpData 所在对象），服务端复用与 live 采集
// 完全相同的映射链路，保证两条入口产出的字段口径一致。
type ingestRequest struct {
	Platform string `json:"platform"`
	// Kind 区分列表包与详情包：list（默认）新增/更新列表字段，
	// detail 只补职位描述、招聘者、活跃度等详情字段。
	Kind string `json:"kind"`
	// JobID 是扩展从详情页 URL 取到的岗位 ID：详情响应体不一定带
	// encryptJobId，没有它就定位不到已有岗位。
	JobID string          `json:"jobId"`
	Query string          `json:"query"`
	City  string          `json:"city"`
	Raw   json.RawMessage `json:"raw"`
}

// ingestResponse 返回本批次的落库统计。
type ingestResponse struct {
	OK       bool   `json:"ok"`
	Platform string `json:"platform"`
	Received int    `json:"received"` // 原始条目数
	Mapped   int    `json:"mapped"`   // 成功映射的条目数
	Dropped  int    `json:"dropped"`  // 因缺关键字段被丢弃的条目数
	Updated  int    `json:"updated"`  // 命中已有记录并补全的岗位数
	Added    int    `json:"added"`    // 新增（非覆盖）的岗位数
	Total    int    `json:"total"`    // 落库后总岗位数
}

// handleIngest 接收浏览器扩展上报的岗位数据。
//
// 鉴权与登录会话无关：扩展不在本站域下，拿不到 Cookie，因此用独立的 ingest token
// （保存在 <data>/ingest.token，0600）。token 走请求头，不落 URL，避免进日志。
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	s.applyIngestCORS(w, r)

	if s.st == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "数据存储不可用")
		return
	}
	username, ok := s.resolveIngestUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "上报令牌无效")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, ingestBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req ingestRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法的 JSON："+err.Error())
		return
	}

	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少 platform")
		return
	}
	pcfg := s.cfg.Platform(platform)
	if pcfg == nil {
		writeError(w, http.StatusBadRequest, "UNKNOWN_PLATFORM", "未知平台："+platform)
		return
	}
	if len(req.Raw) == 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少 raw")
		return
	}

	var root map[string]any
	if err := json.Unmarshal(req.Raw, &root); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "raw 必须是 JSON 对象："+err.Error())
		return
	}

	spec := specFromPlatform(pcfg)
	ctx := mapping.Context{Keyword: req.Query, QueryCity: req.City}

	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "list"
	}
	if kind == "detail" {
		s.ingestDetail(w, req, pcfg, spec, ctx, root, platform, username)
		return
	}

	items := extractIngestItems(root, pcfg.ListPath)
	if len(items) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "NO_ITEMS", "raw 中没有可识别的岗位数组")
		return
	}

	jobs := make([]model.Job, 0, len(items))
	dropped := 0
	for _, item := range items {
		draft := mapping.BuildDraft(item, spec)
		// 城市兜底：原始条目偶尔缺 cityName，用本次上报声明的城市补位，
		// 避免落库后出现"城市未知"的岗位。
		if strings.TrimSpace(draft.CityName) == "" {
			draft.CityName = strings.TrimSpace(req.City)
		}
		job := mapping.Finalize(draft, spec, ctx)
		if strings.TrimSpace(job.JobName) == "" || strings.TrimSpace(job.BrandName) == "" {
			dropped++
			continue
		}
		jobs = append(jobs, job)
	}

	added, total, err := s.lib.Merge(username, jobs)
	if err != nil {
		s.log.Error("ingest 落库失败", "err", err, "platform", platform)
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "写入岗位数据失败")
		return
	}

	s.log.Info("接收扩展上报",
		"platform", platform, "query", req.Query, "city", req.City,
		"received", len(items), "mapped", len(jobs), "dropped", dropped, "added", added, "total", total)

	writeJSON(w, http.StatusOK, ingestResponse{
		OK:       true,
		Platform: platform,
		Received: len(items),
		Mapped:   len(jobs),
		Dropped:  dropped,
		Added:    added,
		Total:    total,
	})
}

// handleIngestPreflight 处理跨域预检。
//
// 扩展从 www.zhipin.com 页面发起请求，属于跨域，浏览器会先发 OPTIONS。
func (s *Server) handleIngestPreflight(w http.ResponseWriter, r *http.Request) {
	s.applyIngestCORS(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// applyIngestCORS 只为上报接口放行跨域来源。
//
// 允许集合是白名单而非反射任意 Origin：令牌虽然能兜底，但把 CORS 收窄可以
// 减少恶意页面诱导用户浏览器发包的面。
func (s *Server) applyIngestCORS(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || !ingestOriginAllowed(origin) {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, X-Ingest-Token, Authorization")
	h.Set("Access-Control-Max-Age", "600")
}

// ingestOriginAllowed 判断来源是否在允许集合内。
func ingestOriginAllowed(origin string) bool {
	switch origin {
	case "https://www.zhipin.com", "https://zhipin.com", "https://m.zhipin.com":
		return true
	}
	// 本地调试：允许 127.0.0.1 与 localhost 的任意端口。
	for _, p := range []string{"http://127.0.0.1:", "http://localhost:"} {
		if strings.HasPrefix(origin, p) {
			return true
		}
	}
	return false
}

// extractIngestItems 从原始响应中取出岗位数组。
//
// 主路径用平台配置里的 listPath；同时兼容几种常见变体，避免因为扩展上报的是
// 推荐流而不是搜索结果时被判成"没有数据"。
func extractIngestItems(root map[string]any, listPath string) []map[string]any {
	candidates := make([]string, 0, 6)
	if p := strings.TrimSpace(listPath); p != "" {
		candidates = append(candidates, p)
		// listPath 常见形态是 zpData.jobList，补上两种裁剪。
		if i := strings.LastIndex(p, "."); i > 0 {
			candidates = append(candidates, p[i+1:])
		}
		if strings.HasPrefix(p, "zpData.") {
			candidates = append(candidates, "zpData."+strings.TrimPrefix(p, "zpData."))
		}
	}
	candidates = append(candidates, "zpData.jobList", "jobList", "zpData.list", "data.list")

	seen := map[string]bool{}
	for _, path := range candidates {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		v, ok := mapping.ReadPath(root, path)
		if !ok {
			continue
		}
		if items := toMapSlice(v); len(items) > 0 {
			return items
		}
	}
	return nil
}

// toMapSlice 把 []any 收敛成 []map[string]any，非对象元素直接丢弃。
func toMapSlice(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok && len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// ingestDetail 处理详情包：只补全 JD、招聘者、活跃度等详情字段。
//
// 详情接口的响应结构是 {zpData:{jobInfo:{...}}}，与列表的 jobList 不同，
// 因此单独走一条路径；匹配已有岗位靠 externalId（encryptJobId），
// 拿不到 ID 就无法定位，直接拒绝而不是插一条重复岗位。
func (s *Server) ingestDetail(
	w http.ResponseWriter,
	req ingestRequest,
	pcfg *config.Platform,
	spec mapping.Spec,
	ctx mapping.Context,
	root map[string]any,
	platform string,
	username string,
) {
	info, ok := mapping.ReadPath(root, "zpData.jobInfo")
	if !ok {
		info, ok = mapping.ReadPath(root, "jobInfo")
	}
	item, isMap := info.(map[string]any)
	if !ok || !isMap || len(item) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "NO_ITEMS", "raw 中没有 zpData.jobInfo")
		return
	}

	draft := mapping.BuildDraft(item, spec)
	if strings.TrimSpace(draft.CityName) == "" {
		draft.CityName = strings.TrimSpace(req.City)
	}
	job := mapping.Finalize(draft, spec, ctx)
	// 详情包自带发布时间时，标记来源为 detail——否则会被误标成 api，
	// 排查时看不出这条时间是列表给的还是点开详情补的。
	if strings.TrimSpace(job.PublishedAt) != "" {
		job.PublishedAtSource = "detail"
	}

	// 详情响应体不一定带 encryptJobId，优先用扩展从 URL 取到的 jobId 兜底。
	if strings.TrimSpace(job.ExternalID) == "" {
		if v := strings.TrimSpace(req.JobID); v != "" {
			job.ExternalID = v
			job.ID = mapping.MakeID(job.Platform, job.ExternalID, mapping.Fingerprint(job))
		}
	}
	if strings.TrimSpace(job.ExternalID) == "" {
		// 只打字段名不打值：便于定位缺哪个字段，又不把岗位内容写进日志。
		s.log.Warn("详情缺少岗位 ID", "platform", platform, "fields", sortedKeys(item))
		writeError(w, http.StatusUnprocessableEntity, "NO_JOB_ID",
			"详情缺少岗位 ID：响应里既没有 encryptJobId，请求也没有带 jobId")
		return
	}

	updated, added, total, err := s.lib.UpsertDetails(username, []model.Job{job})
	if err != nil {
		s.log.Error("ingest 详情落库失败", "err", err, "platform", platform, "id", job.ID)
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "写入岗位数据失败")
		return
	}

	s.log.Info("接收详情上报", "platform", platform, "id", job.ID,
		"hasDescription", strings.TrimSpace(job.Description) != "", "updated", updated, "added", added)

	writeJSON(w, http.StatusOK, ingestResponse{
		OK:       true,
		Platform: platform,
		Received: 1,
		Mapped:   1,
		Updated:  updated,
		Added:    added,
		Total:    total,
	})
}

// specFromPlatform 把平台配置转成映射层需要的 Spec。
//
// 单独抽出来是为了有一处可测的「配置透传」入口：此前新增的
// activeOnlinePath / activeTimePath 只接到了 CLI 采集，这里漏传，
// 导致扩展上报的数据活跃状态全为空。
func specFromPlatform(p *config.Platform) mapping.Spec {
	return mapping.Spec{
		Platform:         p.ID,
		PlatformName:     p.Name,
		FieldMap:         p.FieldMap,
		PublishTimePaths: p.PublishTimePaths,
		JobURLTemplate:   p.JobURLTemplate,
		ActiveOnlinePath: p.ActiveOnlinePath,
		ActiveTimePath:   p.ActiveTimePath,
	}
}

// resolveIngestUser 用上报令牌反查账号。
//
// 令牌是账号级凭据：同一个部署里每个账号一把，互不通用。
// 这里不接受会话 Cookie —— 扩展运行在第三方域名下，本来就拿不到。
func (s *Server) resolveIngestUser(r *http.Request) (string, bool) {
	if s.creds == nil {
		return "", false
	}

	got := strings.TrimSpace(r.Header.Get("X-Ingest-Token"))
	if got == "" {
		if authz := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(authz, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
		}
	}
	if got == "" {
		return "", false
	}
	return s.creds.Resolve(got)
}

// sortedKeys 返回 map 的键名（排序后），仅用于日志定位缺字段。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 40 {
		keys = keys[:40]
	}
	return keys
}
