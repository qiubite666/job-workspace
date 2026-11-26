package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
	Kind  string          `json:"kind"`
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
	if !s.ingestTokenOK(r) {
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

	spec := mapping.Spec{
		Platform:         pcfg.ID,
		PlatformName:     pcfg.Name,
		FieldMap:         pcfg.FieldMap,
		PublishTimePaths: pcfg.PublishTimePaths,
		JobURLTemplate:   pcfg.JobURLTemplate,
	}
	ctx := mapping.Context{Keyword: req.Query, QueryCity: req.City}

	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "list"
	}
	if kind == "detail" {
		s.ingestDetail(w, req, pcfg, spec, ctx, root, platform)
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

	added, total, err := s.st.MergeAndSave(jobs)
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

// ingestTokenOK 恒定时间比较上报令牌。
func (s *Server) ingestTokenOK(r *http.Request) bool {
	want := strings.TrimSpace(s.ingestToken)
	if want == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Ingest-Token"))
	if got == "" {
		// 兼容 Authorization: Bearer <token>
		if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
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

// ingestTokenPath 返回令牌文件路径。
func ingestTokenPath(dataDir string) string {
	return filepath.Join(dataDir, "ingest.token")
}

// loadOrCreateIngestToken 读取上报令牌，不存在则生成。
//
// 令牌只写文件、不出现在日志与接口响应里；运维需要时自行 cat 该文件。
func loadOrCreateIngestToken(dataDir string) (string, error) {
	path := ingestTokenPath(dataDir)
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
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

	if strings.TrimSpace(job.ExternalID) == "" {
		writeError(w, http.StatusUnprocessableEntity, "NO_JOB_ID", "详情缺少岗位 ID，无法匹配已有记录")
		return
	}

	updated, added, total, err := s.st.UpsertDetails([]model.Job{job})
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
