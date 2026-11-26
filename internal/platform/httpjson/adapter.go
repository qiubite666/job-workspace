package httpjson

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"jobworkspace/internal/errs"
	"jobworkspace/internal/httpx"
	"jobworkspace/internal/mapping"
	"jobworkspace/internal/model"
	"jobworkspace/internal/platform"
)

// 默认每页条数：Spec.PageSize 未配置时的兜底值，与内置配置的 defaults.pageSize 保持一致。
const defaultPageSize = 30

// maxPageSize 是页大小硬上限，防止调用方传 9999 把平台打爆。
const maxPageSize = 100

// Adapter 是 Spec 的运行时实例，实现 platform.Adapter。
//
// 无状态（除只读的 spec 外不持有任何字段），因此可以被多个采集任务并发使用。
type Adapter struct {
	spec Spec
}

// New 依据 Spec 构造适配器。
func New(spec Spec) *Adapter {
	return &Adapter{spec: spec}
}

// ID 返回平台标识。
func (a *Adapter) ID() string { return a.spec.ID }

// Meta 返回平台元信息；Name 为空时退化为 ID，避免前端出现空白标题。
func (a *Adapter) Meta() platform.Meta {
	name := strings.TrimSpace(a.spec.Name)
	if name == "" {
		name = a.spec.ID
	}
	return platform.Meta{
		Name:         name,
		Domain:       a.spec.Domain,
		Experimental: a.spec.Experimental,
	}
}

// Capabilities 声明能力：httpjson 引擎只做列表采集，不做详情补全。
func (a *Adapter) Capabilities() platform.Capabilities {
	return platform.Capabilities{List: true, Detail: false}
}

// NormalizeParams 兜底翻页参数。
//
// 约定：model.Params.Pages 在单次 FetchList 调用中表示**当前页号**（1-based），
// 由编排层逐页递推；这里的下限兜底保证适配器被单独调用时也不会请求第 0 页。
//
// 刻意**不**按 MaxPages 截断 Pages：翻页上限属于编排层决策（它还要用 Pages<=0 表示
// 「取配置默认页数」），适配器擅自截断会让调用方误以为只需翻 1 页。
// 城市名到城市码的转换放在 FetchList，避免污染上报给前端的原始参数。
func (a *Adapter) NormalizeParams(p model.Params) model.Params {
	if p.Pages < 1 {
		p.Pages = 1
	}
	size := a.spec.PageSize
	if size <= 0 {
		size = defaultPageSize
	}
	if p.PageSize < 1 {
		p.PageSize = size
	}
	if p.PageSize > maxPageSize {
		p.PageSize = maxPageSize
	}
	return p
}

// FetchList 拉取一页列表。
//
// 处理顺序（与契约 §6.2 一致）：
//  1. 参数兜底 + 登录态检查（仅 live 模式拦截）
//  2. 城市名命中 Cities 则换成城市码，否则原样透传
//  3. 渲染 QueryParams 占位符并拼 URL
//  4. GetJSON
//  5. 业务码校验（必须返回 error）
//  6. 取列表数组、取 hasMore
//
// 列表为空且无错误时返回空 Items，由编排层据此停止翻页。
func (a *Adapter) FetchList(ctx context.Context, rt *platform.Runtime, p model.Params) (model.ListResult, error) {
	if rt == nil || rt.Client == nil {
		return model.ListResult{}, a.fail(errs.CodeBadConfig, "运行时未注入 HTTP 客户端，无法请求 %s", a.displayID())
	}

	p = a.NormalizeParams(p)

	if err := a.checkCredentials(p); err != nil {
		return model.ListResult{}, err
	}

	cityCode := resolveCity(a.spec.Cities, p.City)

	rawURL, err := BuildURL(a.spec.SearchURL, a.spec.QueryParams, p, cityCode)
	if err != nil {
		return model.ListResult{}, a.fail(errs.CodeBadConfig, "构造 %s 请求地址失败: %v", a.displayID(), err)
	}
	if rt.Log != nil {
		rt.Log.Debug("平台请求", "platform", a.spec.ID, "page", p.Pages, "url", rawURL)
	}

	body, resp, err := rt.Client.GetJSON(ctx, rawURL)

	var topArray []any // 顶层数组兜底（部分平台的 searchUrl 直接返回 [ ... ]）
	if err != nil {
		if resp != nil && resp.Status >= 200 && resp.Status < 300 && len(resp.Body) > 0 {
			if jsonErr := json.Unmarshal(resp.Body, &topArray); jsonErr == nil {
				body, err = nil, nil
			}
		}
		if err != nil {
			return model.ListResult{}, a.normalizeError(err, resp, rawURL)
		}
	}

	if body != nil {
		if err := a.checkBizCode(body); err != nil {
			return model.ListResult{}, err
		}
	}

	items, skipped, err := a.extractItems(body, topArray)
	if err != nil {
		return model.ListResult{}, err
	}
	if skipped > 0 && rt.Log != nil {
		rt.Log.Warn("列表中存在非对象条目，已跳过", "platform", a.spec.ID, "skipped", skipped)
	}

	// 本页无数据：明确返回空切片（非 nil），让编排层停止翻页。
	if len(items) == 0 {
		return model.ListResult{Items: []map[string]any{}}, nil
	}

	return model.ListResult{Items: items, HasMore: a.hasMore(body)}, nil
}

// MapItem 把平台原始条目映射为统一草稿。
//
// 映射本身不返回 error（字段缺失一律降级为空值）；只有平台必须在 PostMap 里抛错的情况
// 才会非 nil —— 当前实现始终返回 nil error。
func (a *Adapter) MapItem(rt *platform.Runtime, raw map[string]any) (model.JobDraft, error) {
	if raw == nil {
		return model.JobDraft{}, nil
	}
	draft := mapping.BuildDraft(raw, mapping.Spec{
		Platform:         a.spec.ID,
		PlatformName:     a.Meta().Name,
		FieldMap:         a.spec.FieldMap,
		PublishTimePaths: a.spec.PublishTimePaths,
		JobURLTemplate:   a.spec.JobURLTemplate,
	})
	if a.spec.PostMap != nil {
		a.spec.PostMap(rt, &draft, raw)
	}
	return draft, nil
}

// BuildURL 渲染查询参数并拼出完整请求地址。
//
// query 的 value 支持四个占位符：{{query}} {{city}} {{page}} {{pageSize}}；
// cityCode 为空时退化为 p.City 原值（中文城市名直接用 url.Values 编码，不报错）。
// 参数按 key 排序后写入，保证同一输入产出同一 URL（便于日志比对与缓存）。
func BuildURL(endpoint string, query map[string]string, p model.Params, cityCode string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("searchUrl 未配置")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("searchUrl 非法 %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("searchUrl 必须是绝对地址，当前为 %q", endpoint)
	}

	page := p.Pages
	if page < 1 {
		page = 1
	}
	size := p.PageSize
	if size <= 0 {
		size = defaultPageSize
	}
	city := strings.TrimSpace(cityCode)
	if city == "" {
		city = strings.TrimSpace(p.City)
	}

	// 保留 searchUrl 自带的查询参数（如固定的 scene），配置文件里的同名 key 优先覆盖。
	values := u.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		values.Set(k, renderTemplate(query[k], p.Query, city, page, size))
	}
	u.RawQuery = values.Encode()

	return u.String(), nil
}

// ---------- 内部实现 ----------

// displayID 返回日志/错误里展示的平台标识。
func (a *Adapter) displayID() string {
	if id := strings.TrimSpace(a.spec.ID); id != "" {
		return id
	}
	return "未知平台"
}

// fail 构造带平台标识的 PlatformError。
func (a *Adapter) fail(code errs.Code, format string, args ...any) *errs.PlatformError {
	e := errs.New(code, format, args...)
	e.Platform = a.spec.ID
	return e
}

// checkCredentials 在 live 模式下校验登录态。
//
// 判定顺序：CLI 传入的 cookie → CookieEnv 指向的环境变量。
// 两者都为空且处于 live 模式 → AUTH_REQUIRED，避免发一次注定失败的请求。
// sample/local 模式不发起网络请求，直接放行。
func (a *Adapter) checkCredentials(p model.Params) error {
	env := strings.TrimSpace(a.spec.CookieEnv)
	if env == "" {
		return nil
	}
	if strings.TrimSpace(p.Cookie) != "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv(env)) != "" {
		return nil
	}
	// 只有显式声明 live 才拦截；mode 为空时调用方可能是在做本地映射测试。
	if strings.ToLower(strings.TrimSpace(p.Mode)) != "live" {
		return nil
	}
	return a.fail(errs.CodeAuthRequired, "缺少 %s，请通过 --cookie 或环境变量提供登录态", env)
}

// checkBizCode 校验业务码，失败时返回 *errs.PlatformError。
//
// 判定顺序：SuccessCodes 白名单 → ErrorCodes 显式映射 → errs.ClassifyPayload → UNKNOWN。
func (a *Adapter) checkBizCode(body map[string]any) error {
	if strings.TrimSpace(a.spec.CodeField) == "" {
		return nil
	}
	codeVal, ok := mapping.ReadPath(body, a.spec.CodeField)
	if !ok {
		return a.fail(errs.CodeParseFailed, "响应缺少业务码字段 %q", a.spec.CodeField)
	}
	codeStr := strings.TrimSpace(mapping.ReadString(body, a.spec.CodeField))

	if a.isSuccessCode(codeStr) {
		return nil
	}

	msg := ""
	if a.spec.MessageField != "" {
		msg = strings.TrimSpace(mapping.ReadString(body, a.spec.MessageField))
	}

	code := a.mapErrorCode(codeStr)
	if code == "" {
		if classified, ok := errs.ClassifyPayload(codeVal, msg); ok {
			code = classified
		}
	}
	if code == "" {
		code = errs.CodeUnknown
	}

	detail := fmt.Sprintf("业务码 %s", codeStr)
	if codeStr == "" {
		detail = "业务码为空"
	}
	if msg != "" {
		detail += ": " + msg
	}
	return a.fail(code, "%s", detail)
}

// isSuccessCode 判断业务码是否成功。
//
// SuccessCodes 为空时按「code == 0」判定（覆盖 0 / "0" / 0.0 等形态）。
func (a *Adapter) isSuccessCode(codeStr string) bool {
	if len(a.spec.SuccessCodes) == 0 {
		return codeStr == "0"
	}
	for _, want := range a.spec.SuccessCodes {
		if strings.EqualFold(strings.TrimSpace(want), codeStr) {
			return true
		}
	}
	return false
}

// mapErrorCode 查显式业务码映射；未命中返回空串。
func (a *Adapter) mapErrorCode(codeStr string) errs.Code {
	if len(a.spec.ErrorCodes) == 0 || codeStr == "" {
		return ""
	}
	raw, ok := a.spec.ErrorCodes[codeStr]
	if !ok {
		return ""
	}
	c := strings.ToUpper(strings.TrimSpace(raw))
	if c == "" {
		return ""
	}
	return errs.Code(c)
}

// extractItems 从响应里取出列表数组，并统计被跳过的非对象元素。
//
// topArray 非 nil 表示响应本身就是顶层数组；否则按 ListPath 取值。
func (a *Adapter) extractItems(body map[string]any, topArray []any) ([]map[string]any, int, error) {
	if topArray != nil {
		items, skipped := toItems(topArray)
		return items, skipped, nil
	}

	path := strings.TrimSpace(a.spec.ListPath)
	if path == "" {
		return nil, 0, a.fail(errs.CodeParseFailed, "响应不是顶层数组，且未配置 listPath")
	}
	v, ok := mapping.ReadPath(body, path)
	if !ok {
		return nil, 0, a.fail(errs.CodeParseFailed, "响应缺少列表字段 %q", path)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, 0, a.fail(errs.CodeParseFailed, "列表字段 %q 不是数组（实际类型 %T）", path, v)
	}
	items, skipped := toItems(arr)
	return items, skipped, nil
}

// hasMore 读取「是否还有下一页」；字段缺失或类型不符一律视为 false。
func (a *Adapter) hasMore(body map[string]any) bool {
	path := strings.TrimSpace(a.spec.HasMorePath)
	if body == nil || path == "" {
		return false
	}
	v, ok := mapping.ReadPath(body, path)
	if !ok {
		return false
	}
	return toBool(v)
}

// normalizeError 把任意错误收敛为 *errs.PlatformError 并补上平台标识。
func (a *Adapter) normalizeError(err error, resp *httpx.Response, rawURL string) error {
	if pe, ok := errs.As(err); ok {
		if pe.Platform == "" {
			pe.Platform = a.spec.ID
		}
		return pe
	}

	status := 0
	if resp != nil {
		status = resp.Status
	}
	if code, ok := errs.ClassifyStatus(status); ok {
		e := errs.Wrap(code, err, "请求 %s 失败", rawURL)
		e.Status = status
		e.Platform = a.spec.ID
		return e
	}
	e := errs.Wrap(errs.CodeUnknown, err, "请求 %s 失败", rawURL)
	e.Platform = a.spec.ID
	return e
}

// renderTemplate 替换查询参数模板里的占位符。
func renderTemplate(tpl, query, city string, page, size int) string {
	if !strings.Contains(tpl, "{{") {
		return tpl
	}
	return strings.NewReplacer(
		"{{query}}", query,
		"{{city}}", city,
		"{{page}}", strconv.Itoa(page),
		"{{pageSize}}", strconv.Itoa(size),
	).Replace(tpl)
}

// resolveCity 把城市名换成城市码；未命中时原样返回（中文城市名直接透传，不报错）。
func resolveCity(cities map[string]string, city string) string {
	city = strings.TrimSpace(city)
	if city == "" || len(cities) == 0 {
		return city
	}
	if code, ok := cities[city]; ok && strings.TrimSpace(code) != "" {
		return strings.TrimSpace(code)
	}
	return city
}

// toItems 把 []any 转成 []map[string]any，非对象元素跳过并计数。
func toItems(arr []any) ([]map[string]any, int) {
	items := make([]map[string]any, 0, len(arr))
	skipped := 0
	for _, v := range arr {
		m, ok := v.(map[string]any)
		if !ok {
			skipped++
			continue
		}
		items = append(items, m)
	}
	return items, skipped
}

// toBool 兼容 bool 与字符串形态的布尔值。
func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "y":
			return true
		}
		return false
	case float64:
		return x != 0
	case int:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	default:
		return false
	}
}
