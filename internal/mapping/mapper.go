// Package mapping 负责「平台原始条目 → 统一岗位模型」的字段映射与收口。
//
// 分工约定：
//   - 适配器只提供扁平的 model.JobDraft（取值、拼 URL）；
//   - 薪资解析、发布时间解析委托给 internal/parser，保证全平台口径一致；
//   - 本包不做任何网络请求，也不返回 error：字段缺失一律降级为空值。
package mapping

import (
	"crypto/sha1" // 仅用于生成确定性 ID 摘要，非安全用途
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"jobworkspace/internal/model"
	"jobworkspace/internal/parser"
)

// Spec 描述一个平台的映射规则。
//
// FieldMap 的 key 为 canonical 字段名（externalId/jobName/brandName/...），
// value 为候选点路径数组，按序取第一个非空值。
type Spec struct {
	Platform         string              // 平台标识，如 boss
	PlatformName     string              // 平台中文名，如 BOSS直聘
	FieldMap         map[string][]string // canonical 字段 -> 候选路径
	PublishTimePaths []string            // 发布时间候选路径
	JobURLTemplate   string              // 支持 {{externalId}} 占位
}

// Context 是收口阶段的外部上下文（来自一次采集的参数与时间）。
type Context struct {
	Keyword     string // 采集关键词
	QueryCity   string // 采集城市
	CollectedAt string // 采集时间 RFC3339；为空时自动取当前时间
}

// canonical 字段名常量，避免 FieldMap 的 key 拼写漂移。
const (
	fieldExternalID       = "externalId"
	fieldJobName          = "jobName"
	fieldBrandName        = "brandName"
	fieldCityName         = "cityName"
	fieldAreaDistrict     = "areaDistrict"
	fieldBusinessDistrict = "businessDistrict"
	fieldSalaryDesc       = "salaryDesc"
	fieldExperience       = "experience"
	fieldDegree           = "degree"
	fieldJobLabels        = "jobLabels"
	fieldSkills           = "skills"
	fieldWelfareList      = "welfareList"
	fieldBossName         = "bossName"
	fieldBossTitle        = "bossTitle"
	fieldActiveTimeDesc   = "activeTimeDesc"
	fieldDescription      = "description"
	fieldJobURL           = "jobUrl"
	fieldBrandIndustry    = "brandIndustry"
	fieldBrandScaleName   = "brandScaleName"
	fieldBrandStageName   = "brandStageName"
)

// ReadPath 按点路径读取嵌套值，支持数组下标（如 a.b.0.c）。
//
// 任一层缺失、类型不符或下标越界都返回 (nil, false)，不会 panic。
// path 为空串或含空段时同样返回 false。
func ReadPath(obj map[string]any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" || obj == nil {
		return nil, false
	}

	var cur any = obj
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil, false
		}
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []map[string]any:
			// 兼容非 JSON 反序列化来源（如手工构造的样例数据）。
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// ReadString 按点路径读取并转换为字符串。
//
// 命中且转换后非空才返回值；数值转字符串时整数不带小数点（1795000000000 不会变成科学计数法），
// 布尔值转为 "true"/"false"。其余类型（对象、数组、nil）返回空串。
func ReadString(obj map[string]any, path string) string {
	v, ok := ReadPath(obj, path)
	if !ok {
		return ""
	}
	s, _ := scalarString(v)
	return s
}

// ReadStrings 读取字符串数组。
//
// 支持三种原始形态：`["a","b"]`、单个字符串、以及元素为对象时依次取
// name/title/keyword/value 字段。无有效值时返回空切片而非 nil。
func ReadStrings(obj map[string]any, path string) []string {
	v, ok := ReadPath(obj, path)
	if !ok {
		return []string{}
	}
	return toStrings(v)
}

// BuildDraft 依据 Spec 把原始条目扁平化为 Draft。
//
// 只做取值与 URL 拼装，不解析薪资、不解析时间、不生成 ID。
func BuildDraft(raw map[string]any, spec Spec) model.JobDraft {
	d := model.JobDraft{
		ExternalID:       pickString(raw, spec, fieldExternalID),
		JobName:          pickString(raw, spec, fieldJobName),
		BrandName:        pickString(raw, spec, fieldBrandName),
		CityName:         pickString(raw, spec, fieldCityName),
		AreaDistrict:     pickString(raw, spec, fieldAreaDistrict),
		BusinessDistrict: pickString(raw, spec, fieldBusinessDistrict),
		SalaryDesc:       pickString(raw, spec, fieldSalaryDesc),
		Experience:       pickString(raw, spec, fieldExperience),
		Degree:           pickString(raw, spec, fieldDegree),
		JobLabels:        pickStrings(raw, spec, fieldJobLabels),
		Skills:           pickStrings(raw, spec, fieldSkills),
		WelfareList:      pickStrings(raw, spec, fieldWelfareList),
		BossName:         pickString(raw, spec, fieldBossName),
		BossTitle:        pickString(raw, spec, fieldBossTitle),
		ActiveTimeDesc:   pickString(raw, spec, fieldActiveTimeDesc),
		Description:      pickString(raw, spec, fieldDescription),
		BrandIndustry:    pickString(raw, spec, fieldBrandIndustry),
		BrandScaleName:   pickString(raw, spec, fieldBrandScaleName),
		BrandStageName:   pickString(raw, spec, fieldBrandStageName),
		PublishedRaw:     pickPublishRaw(raw, spec.PublishTimePaths),
	}

	d.JobURL = buildJobURL(raw, spec, d.ExternalID)

	// Extra 保持 nil：原始条目体积大，明细留给适配器 PostMap 自行取用，
	// 不随 Job 落盘，避免 data/jobs.json 膨胀。
	return d
}

// Finalize 把 Draft 收口为可落盘的 Job。
//
// 负责：薪资解析、发布时间解析、Location 拼接、ID 生成、nil slice/空值收敛。
// CollectedAt 为空时取当前时间，非空的可解析 RFC3339 统一转 UTC，保证全工程
// 时间戳都是 Z 结尾定长串。任何字段缺失都不会 panic，也不会返回 error；
// 解析失败按 unknown/missing 降级。
func Finalize(d model.JobDraft, spec Spec, ctx Context) model.Job {
	now := time.Now()

	j := model.Job{
		Platform:         strings.TrimSpace(spec.Platform),
		PlatformName:     strings.TrimSpace(spec.PlatformName),
		ExternalID:       strings.TrimSpace(d.ExternalID),
		JobName:          strings.TrimSpace(d.JobName),
		BrandName:        strings.TrimSpace(d.BrandName),
		CityName:         strings.TrimSpace(d.CityName),
		AreaDistrict:     strings.TrimSpace(d.AreaDistrict),
		BusinessDistrict: strings.TrimSpace(d.BusinessDistrict),
		SalaryDesc:       strings.TrimSpace(d.SalaryDesc),
		Experience:       strings.TrimSpace(d.Experience),
		Degree:           strings.TrimSpace(d.Degree),
		JobLabels:        cleanSlice(d.JobLabels),
		Skills:           cleanSlice(d.Skills),
		WelfareList:      cleanSlice(d.WelfareList),
		BossName:         strings.TrimSpace(d.BossName),
		BossTitle:        strings.TrimSpace(d.BossTitle),
		ActiveTimeDesc:   strings.TrimSpace(d.ActiveTimeDesc),
		Description:      plainText(d.Description),
		JobURL:           strings.TrimSpace(d.JobURL),
		BrandIndustry:    strings.TrimSpace(d.BrandIndustry),
		BrandScaleName:   strings.TrimSpace(d.BrandScaleName),
		BrandStageName:   strings.TrimSpace(d.BrandStageName),
		Keyword:          strings.TrimSpace(ctx.Keyword),
		QueryCity:        strings.TrimSpace(ctx.QueryCity),
	}

	// 薪资：原文保留，解析结果与展示文本分开存放。
	salary := parser.ParseSalary(j.SalaryDesc)
	j.SalaryMin = salary.Min
	j.SalaryMax = salary.Max
	j.SalaryMonths = salary.Months
	j.SalaryUnit = salary.Unit
	j.SalaryKind = salary.Kind
	j.SalaryMonthlyMin = salary.MonthlyMin
	j.SalaryMonthlyMax = salary.MonthlyMax
	j.SalaryText = parser.FormatSalary(salary)

	// 发布时间：候选值由适配器按优先级收集，这里只做解析与来源归一。
	iso, source := parser.ParsePublishTime(d.PublishedRaw, now)
	j.PublishedAt = iso
	j.PublishedAtSource = normalizeSource(iso, source)

	j.Location = joinLocation(j.CityName, j.AreaDistrict, j.BusinessDistrict)

	j.CollectedAt = normalizeCollectedAt(ctx.CollectedAt, now)

	// ID 依赖指纹，必须在其余字段收敛之后计算。
	j.ID = MakeID(j.Platform, j.ExternalID, Fingerprint(j))

	return j
}

// Fingerprint 生成去重指纹：`公司|岗位|城市|行政区`。
//
// 每段先 NormalizeText 再转小写（英文大小写不同视为同一岗位，中文不受影响），
// 段内不做其他改写，保证指纹稳定可复现。
func Fingerprint(j model.Job) string {
	parts := [4]string{j.BrandName, j.JobName, j.CityName, j.AreaDistrict}
	for i, p := range parts {
		parts[i] = strings.ToLower(NormalizeText(p))
	}
	return strings.Join(parts[:], "|")
}

// MakeID 生成全局唯一 ID。
//
// externalID 非空时直接用 `platform:externalID`；否则退化为 `platform:` + sha1(指纹) 前 16 位十六进制。
// 指纹为空（四个字段全空）时仍能生成稳定 ID，但去重层会跳过这类记录的指纹合并。
func MakeID(platform, externalID, fp string) string {
	platform = strings.TrimSpace(platform)
	externalID = strings.TrimSpace(externalID)
	if externalID != "" {
		return platform + ":" + externalID
	}
	sum := sha1.Sum([]byte(platform + "|" + fp))
	return platform + ":" + hex.EncodeToString(sum[:])[:16]
}

// Completeness 计算岗位信息的完整度得分（0~100）。
//
// 权重合计恰好 100，用于去重时择优保留信息更全的一条：
// 岗位名 15、公司 12、有效薪资 15、城市 8、经验 6、学历 6、技能 8、
// 标签 5、链接 8、发布时间 7、福利 5、招聘者 5。
func Completeness(j model.Job) int {
	score := 0
	if strings.TrimSpace(j.JobName) != "" {
		score += 15
	}
	if strings.TrimSpace(j.BrandName) != "" {
		score += 12
	}
	if strings.TrimSpace(j.SalaryDesc) != "" && j.SalaryKind != "" && j.SalaryKind != "unknown" {
		score += 15
	}
	if strings.TrimSpace(j.CityName) != "" {
		score += 8
	}
	if strings.TrimSpace(j.Experience) != "" {
		score += 6
	}
	if strings.TrimSpace(j.Degree) != "" {
		score += 6
	}
	if len(j.Skills) > 0 {
		score += 8
	}
	if len(j.JobLabels) > 0 {
		score += 5
	}
	if strings.TrimSpace(j.JobURL) != "" {
		score += 8
	}
	if strings.TrimSpace(j.PublishedAt) != "" {
		score += 7
	}
	if len(j.WelfareList) > 0 {
		score += 5
	}
	if strings.TrimSpace(j.BossName) != "" {
		score += 5
	}
	return score
}

// NormalizeText 归一化文本，用于指纹与去重比较。
//
// 处理顺序：trim → 全角转半角（含全角空格 U+3000）→ 括号/冒号/逗号统一为半角
// → 连续空白压缩为单个空格。不做大小写折叠，展示值不受影响。
func NormalizeText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			// 全角 ASCII 区整体右移 0xFEE0 即得半角。
			b.WriteRune(r - 0xFEE0)
		case r == 0x3000: // 全角空格
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}

	// 全角字符已在上面转换，这里兜底处理可能残留的兼容字符。
	s = strings.NewReplacer(
		"（", "(", "）", ")",
		"：", ":", "，", ",",
	).Replace(b.String())

	return strings.Join(strings.Fields(s), " ")
}

// ---------- 内部辅助 ----------

// pickString 按候选路径顺序取第一个非空字符串。
func pickString(raw map[string]any, spec Spec, field string) string {
	for _, p := range spec.FieldMap[field] {
		if s := ReadString(raw, p); s != "" {
			return s
		}
	}
	return ""
}

// pickStrings 按候选路径顺序取第一个非空字符串数组。
func pickStrings(raw map[string]any, spec Spec, field string) []string {
	for _, p := range spec.FieldMap[field] {
		if vs := ReadStrings(raw, p); len(vs) > 0 {
			return vs
		}
	}
	return []string{}
}

// pickPublishRaw 收集发布时间候选值，保持 Spec 中的优先级顺序。
//
// 数字（如毫秒时间戳）转成无小数点的字符串；空值/非标量结构直接跳过，
// 避免把 map 的调试字符串喂给 parser。
func pickPublishRaw(raw map[string]any, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		v, ok := ReadPath(raw, p)
		if !ok {
			continue
		}
		s, ok := scalarString(v)
		if !ok || s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// buildJobURL 渲染详情链接。
//
// 模板为空，或模板依赖 externalId 但条目没有 ID 时，退化为读取原始条目里的
// jobUrl/url/link，保证「拿到手的链接尽量可用」。
func buildJobURL(raw map[string]any, spec Spec, externalID string) string {
	tpl := strings.TrimSpace(spec.JobURLTemplate)
	if tpl != "" {
		if externalID != "" || !strings.Contains(tpl, "{{externalId}}") {
			return strings.TrimSpace(strings.ReplaceAll(tpl, "{{externalId}}", externalID))
		}
	}
	for _, p := range []string{fieldJobURL, "url", "link"} {
		if s := ReadString(raw, p); s != "" {
			return s
		}
	}
	return ""
}

// scalarString 把标量转成展示字符串。
//
// 数值优先按整数输出，避免 1.795e+12 这类科学计数法污染时间戳/薪资文本。
func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return strings.TrimSpace(x), strings.TrimSpace(x) != ""
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64:
		// 'f' 格式永不使用科学计数法，-1 精度输出最短可还原表示：
		// 1795000000000 → "1795000000000"，3.5 → "3.5"。
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case float32:
		return scalarString(float64(x))
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case json.Number:
		s := strings.TrimSpace(x.String())
		return s, s != ""
	default:
		// 对象/数组等复合值不参与字符串字段映射。
		return "", false
	}
}

// toStrings 把任意形态的原始值转换为字符串数组，始终返回非 nil。
func toStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return []string{}
	case string:
		if s := strings.TrimSpace(x); s != "" {
			return []string{s}
		}
		return []string{}
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s := itemString(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		// 单个标量（数字/布尔）也按单元素数组处理。
		if s, ok := scalarString(x); ok && s != "" {
			return []string{s}
		}
		return []string{}
	}
}

// itemString 提取数组元素的文本：字符串直接返回，对象依次尝试 name/title/keyword/value。
func itemString(v any) string {
	if m, ok := v.(map[string]any); ok {
		for _, key := range []string{"name", "title", "keyword", "value"} {
			if s, ok := scalarString(m[key]); ok && s != "" {
				return s
			}
		}
		return ""
	}
	s, ok := scalarString(v)
	if !ok {
		return ""
	}
	return s
}

// cleanSlice 去掉首尾空白与空元素，nil 输入返回空切片（保证 JSON 输出 []）。
func cleanSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// joinLocation 拼接展示地点，任一段为空则跳过该段。
func joinLocation(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "·")
}

// normalizeCollectedAt 收敛采集时间。
//
// 为空取当前时间；可解析的 RFC3339 统一转成 UTC（Z 结尾定长串），使下游
// store/web/前端即使做字符串比较或截取日期也不会错位；无法解析的脏值原样
// 保留，不丢调用方信息（dedupe 会退回字典序兜底）。
func normalizeCollectedAt(raw string, now time.Time) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return now.UTC().Format(time.RFC3339)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

// normalizeSource 把解析来源收敛为 api|detail|missing。
//
// parser 只产出 api/missing；detail 留给未来详情页补采。任何异常组合都以
// 「有时间就是 api、没时间就是 missing」兜底，防止脏值流到前端。
func normalizeSource(iso, source string) string {
	switch source {
	case "detail":
		return "detail"
	case "api":
		if iso == "" {
			return "missing"
		}
		return "api"
	}
	if iso != "" {
		return "api"
	}
	return "missing"
}

// plainText 把职位描述从 HTML/富文本收敛成纯文本。
//
// BOSS 详情接口的 postDescription 常带 <br>、<p>、&nbsp; 等标记，直接塞进
// 前端会破坏排版；这里只做「换行保留 + 标签剥离 + 常见实体解码」，
// 不追求完整的 HTML 解析（也不该在这里做）。
func plainText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// 先把换行类标签变成真正的换行，再剥离其余标签。
	replacer := strings.NewReplacer(
		"<br>", "\n", "<br/>", "\n", "<br />", "\n", "<BR>", "\n",
		"</p>", "\n", "</P>", "\n", "</div>", "\n", "</li>", "\n",
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'",
	)
	s = replacer.Replace(s)

	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}

	// 压缩连续空行：最多保留一个空行，避免正文里出现大片空白。
	lines := strings.Split(b.String(), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
