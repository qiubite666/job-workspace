package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"jobworkspace/internal/model"
	"jobworkspace/internal/parser"
)

// adminBodyLimit 是后台写操作请求体上限。
const adminBodyLimit = 64 << 10

// 单条岗位的可编辑字段上限，对应契约 §3.3。
const (
	maxJobNameLen    = 80
	maxBrandNameLen  = 80
	maxCityNameLen   = 40
	maxDistrictLen   = 40
	maxSalaryLen     = 40
	maxExperienceLen = 20
	maxDegreeLen     = 20
	maxListItems     = 10
	maxListItemLen   = 20
	maxURLLen        = 500
)

// 人工录入岗位的固定来源标识。
const (
	manualPlatform     = "manual"
	manualPlatformName = "手动录入"
	manualKeyword      = "手动录入"
)

// jobInput 是后台新建/编辑岗位的请求体。
type jobInput struct {
	JobName          string   `json:"jobName"`
	BrandName        string   `json:"brandName"`
	CityName         string   `json:"cityName"`
	AreaDistrict     string   `json:"areaDistrict"`
	BusinessDistrict string   `json:"businessDistrict"`
	SalaryDesc       string   `json:"salaryDesc"`
	Experience       string   `json:"experience"`
	Degree           string   `json:"degree"`
	JobLabels        []string `json:"jobLabels"`
	Skills           []string `json:"skills"`
	WelfareList      []string `json:"welfareList"`
	JobURL           string   `json:"jobUrl"`
	PublishedAt      string   `json:"publishedAt"`
}

// handleAdminListJobs 返回后台岗位列表，支持 query/city 过滤与分页参数透传。
//
// 过滤规则与公开接口 GET /api/jobs 一致，但数据源只取 jobs.json：
// 后台管理的是真实业务数据，不能把演示用的示例数据混进来（否则会「列表可见、编辑 404」）。
func (s *Server) handleAdminListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListJobs()
	if err != nil {
		s.log.Warn("后台读取岗位失败", "err", err)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"count": len(out),
		"jobs":  out,
	})
}

// handleAdminGetJob 返回单条岗位详情。
func (s *Server) handleAdminGetJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "岗位不存在")
		return
	}
	if s.st == nil {
		writeError(w, http.StatusInternalServerError, "STORE_READ_FAILED", "读取岗位数据失败")
		return
	}

	job, ok, err := s.st.GetJob(id)
	if err != nil {
		s.log.Warn("后台读取单个岗位失败", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "STORE_READ_FAILED", "读取岗位数据失败")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "岗位不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

// handleAdminCreateJob 新建一条人工录入岗位。
func (s *Server) handleAdminCreateJob(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeJobInput(w, r)
	if !ok {
		return
	}
	if s.st == nil {
		writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "保存岗位失败")
		return
	}

	now := time.Now()
	job, fields := buildManualJob(in, now)
	if len(fields) > 0 {
		writeValidationFailed(w, fields)
		return
	}

	id, err := newManualJobID()
	if err != nil {
		s.log.Error("生成岗位 ID 失败", "err", err)
		writeError(w, http.StatusInternalServerError, "ID_FAILED", "生成岗位 ID 失败")
		return
	}
	job.ID = id
	job.Platform = manualPlatform
	job.PlatformName = manualPlatformName
	job.Keyword = manualKeyword
	job.QueryCity = job.CityName
	job.CollectedAt = now.UTC().Format(time.RFC3339)

	if _, err := s.st.UpsertJob(job); err != nil {
		s.log.Error("保存新岗位失败", "err", err)
		writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "保存岗位失败")
		return
	}

	s.log.Info("后台新增岗位", "id", job.ID, "jobName", job.JobName)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "job": job})
}

// handleAdminUpdateJob 按 ID 整体替换一条岗位的可编辑字段。
func (s *Server) handleAdminUpdateJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "岗位不存在")
		return
	}
	in, ok := decodeJobInput(w, r)
	if !ok {
		return
	}
	if s.st == nil {
		writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "保存岗位失败")
		return
	}

	existing, found, err := s.st.GetJob(id)
	if err != nil {
		s.log.Warn("后台读取待编辑岗位失败", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "STORE_READ_FAILED", "读取岗位数据失败")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "岗位不存在")
		return
	}

	job, fields := buildManualJob(in, time.Now())
	if len(fields) > 0 {
		writeValidationFailed(w, fields)
		return
	}

	// 身份字段不允许被编辑覆盖：ID / Platform / CollectedAt，
	// 以及同属平台身份的 PlatformName / ExternalID。
	job.ID = existing.ID
	job.Platform = existing.Platform
	job.PlatformName = existing.PlatformName
	job.ExternalID = existing.ExternalID
	job.CollectedAt = existing.CollectedAt

	if _, err := s.st.UpsertJob(job); err != nil {
		s.log.Error("保存岗位编辑失败", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "保存岗位失败")
		return
	}

	s.log.Info("后台编辑岗位", "id", job.ID, "jobName", job.JobName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

// handleAdminDeleteJob 按 ID 删除一条岗位。
func (s *Server) handleAdminDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "岗位不存在")
		return
	}
	if s.st == nil {
		writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "删除岗位失败")
		return
	}

	existed, err := s.st.DeleteJob(id)
	if err != nil {
		s.log.Error("删除岗位失败", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "删除岗位失败")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "岗位不存在")
		return
	}

	s.log.Info("后台删除岗位", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeJobInput 解析并做长度/格式以外的结构校验；失败时已写好响应。
func decodeJobInput(w http.ResponseWriter, r *http.Request) (jobInput, bool) {
	var in jobInput
	dec := json.NewDecoder(io.LimitReader(r.Body, adminBodyLimit))
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON")
		return jobInput{}, false
	}
	return in, true
}

// buildManualJob 把请求体收敛成 Job，并按契约 §3.3 校验；
// 校验失败时返回「JSON 字段名 → 中文错误」，此时 Job 内容不可用。
func buildManualJob(in jobInput, now time.Time) (model.Job, map[string]string) {
	fields := make(map[string]string)

	job := model.Job{
		JobName:          strings.TrimSpace(in.JobName),
		BrandName:        strings.TrimSpace(in.BrandName),
		CityName:         strings.TrimSpace(in.CityName),
		AreaDistrict:     strings.TrimSpace(in.AreaDistrict),
		BusinessDistrict: strings.TrimSpace(in.BusinessDistrict),
		SalaryDesc:       strings.TrimSpace(in.SalaryDesc),
		Experience:       strings.TrimSpace(in.Experience),
		Degree:           strings.TrimSpace(in.Degree),
		JobURL:           strings.TrimSpace(in.JobURL),
	}

	requireText(fields, "jobName", job.JobName, "职位名称", maxJobNameLen)
	requireText(fields, "brandName", job.BrandName, "公司名称", maxBrandNameLen)
	requireText(fields, "cityName", job.CityName, "城市", maxCityNameLen)
	limitText(fields, "areaDistrict", job.AreaDistrict, "行政区", maxDistrictLen)
	limitText(fields, "businessDistrict", job.BusinessDistrict, "商圈", maxDistrictLen)
	limitText(fields, "salaryDesc", job.SalaryDesc, "薪资描述", maxSalaryLen)
	limitText(fields, "experience", job.Experience, "经验要求", maxExperienceLen)
	limitText(fields, "degree", job.Degree, "学历要求", maxDegreeLen)
	limitText(fields, "jobUrl", job.JobURL, "职位链接", maxURLLen)

	job.JobLabels = cleanList(in.JobLabels, "jobLabels", "职位标签", fields)
	job.Skills = cleanList(in.Skills, "skills", "技能", fields)
	job.WelfareList = cleanList(in.WelfareList, "welfareList", "福利", fields)

	if job.JobURL != "" && !isHTTPURL(job.JobURL) {
		fields["jobUrl"] = "必须是以 http:// 或 https:// 开头的链接"
	}

	// 薪资：解析结果交给 parser，展示文本用 FormatSalary，绝不自己拼。
	salary := parser.ParseSalary(job.SalaryDesc)
	job.SalaryMin = salary.Min
	job.SalaryMax = salary.Max
	job.SalaryMonths = salary.Months
	job.SalaryUnit = salary.Unit
	job.SalaryKind = salary.Kind
	job.SalaryMonthlyMin = salary.MonthlyMin
	job.SalaryMonthlyMax = salary.MonthlyMax
	job.SalaryText = parser.FormatSalary(salary)

	// 发布时间：YYYY-MM-DD 与 RFC3339 都交给 parser 统一解析。
	published := strings.TrimSpace(in.PublishedAt)
	if published != "" {
		iso, source := parser.ParsePublishTime([]string{published}, now)
		if iso == "" {
			fields["publishedAt"] = "发布时间格式不正确，请使用 YYYY-MM-DD 或 RFC3339"
		} else {
			job.PublishedAt = iso
			job.PublishedAtSource = source
		}
	} else {
		job.PublishedAtSource = "missing"
	}

	job.Location = joinLocation(job.CityName, job.AreaDistrict, job.BusinessDistrict)

	return job, fields
}

// requireText 校验必填文本。
func requireText(fields map[string]string, key, value, label string, maxRunes int) {
	if value == "" {
		fields[key] = label + "不能为空"
		return
	}
	if utf8.RuneCountInString(value) > maxRunes {
		fields[key] = label + "不能超过 " + strconv.Itoa(maxRunes) + " 个字符"
	}
}

// limitText 校验选填文本的长度。
func limitText(fields map[string]string, key, value, label string, maxRunes int) {
	if value != "" && utf8.RuneCountInString(value) > maxRunes {
		fields[key] = label + "不能超过 " + strconv.Itoa(maxRunes) + " 个字符"
	}
}

// cleanList 去空白、去空串、去重并截断到上限；超限时写入 fields。
func cleanList(items []string, key, label string, fields map[string]string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, raw := range items {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if utf8.RuneCountInString(v) > maxListItemLen {
			fields[key] = "每个" + label + "不能超过 " + strconv.Itoa(maxListItemLen) + " 个字符"
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) > maxListItems {
		fields[key] = label + "最多 " + strconv.Itoa(maxListItems) + " 项"
		out = out[:maxListItems]
	}
	return out
}

// isHTTPURL 判断是否为带主机名的 http/https 链接。
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// joinLocation 拼接展示地点，空段自动跳过。
func joinLocation(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			kept = append(kept, v)
		}
	}
	return strings.Join(kept, "·")
}

// newManualJobID 生成 `manual:` + 12 位随机十六进制。
func newManualJobID() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return manualPlatform + ":" + hex.EncodeToString(buf[:]), nil
}

// writeValidationFailed 输出 422 字段级校验错误。
func writeValidationFailed(w http.ResponseWriter, fields map[string]string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"ok":     false,
		"error":  "VALIDATION_FAILED",
		"fields": fields,
	})
}
