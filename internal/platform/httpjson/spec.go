// Package httpjson 是「配置驱动」的通用 JSON 列表采集引擎。
//
// 它把「构造请求 → 校验业务码 → 取列表 → 取是否还有下一页 → 字段映射」这套动作固化下来，
// 于是新增一个 GET + JSON 的平台时，只需要写配置（internal/config/platforms.json），
// 必要时再用 PostMap 补几行平台特有修正，不必复制一遍请求/解析代码。
//
// 边界：
//   - 只处理 GET + JSON 列表接口，不处理 HTML、不处理详情页；
//   - 不解析薪资、不解析发布时间、不算 ID —— 这些统一交给 internal/mapping；
//   - 所有错误都归一为 *errs.PlatformError，编排层据此决定重试/降级/停止。
package httpjson

import (
	"jobworkspace/internal/config"
	"jobworkspace/internal/model"
	"jobworkspace/internal/platform"
)

// Spec 描述一个平台的全部「可配置行为」。
//
// 它与 config.Platform 一一对应，但刻意解耦：适配器只在构造时读一次配置，
// 之后运行期只依赖 Spec，便于单元测试直接构造 Spec 而不必造配置文件。
type Spec struct {
	ID           string // 平台标识
	Name         string // 中文名
	Domain       string // 主域名
	Experimental bool   // 是否为试验性适配器

	SearchURL      string            // 列表接口地址
	QueryParams    map[string]string // 查询参数模板，值支持 {{query}} {{city}} {{page}} {{pageSize}}
	JobURLTemplate string            // 详情链接模板，支持 {{externalId}}
	Referer        string            // 请求 Referer
	Headers        map[string]string // 额外请求头
	CookieEnv      string            // 登录态环境变量名

	ListPath         string // 列表数组点路径，空则期望顶层数组
	HasMorePath      string // 是否还有下一页的布尔点路径
	ActiveOnlinePath string // 招聘者在线状态的布尔点路径
	ActiveTimePath   string // 招聘者最近活跃时间的毫秒时间戳点路径
	CodeField        string // 业务码点路径
	MessageField     string // 业务消息点路径

	SuccessCodes []string          // 视为成功的业务码；空则只有 "0" 成功
	ErrorCodes   map[string]string // 业务码 -> errs.Code 的显式映射

	FieldMap         map[string][]string // 统一字段 -> 候选路径
	PublishTimePaths []string            // 发布时间候选路径
	Cities           map[string]string   // 城市名 -> 城市码

	PageSize int // 每页条数
	MaxPages int // 最大翻页数，0 表示不限制

	// PostMap 可选：平台特有的最终修正钩子，在 mapping.BuildDraft 之后调用。
	// 只允许修正 draft，不得发起网络请求。
	PostMap func(rt *platform.Runtime, draft *model.JobDraft, raw map[string]any)
}

// SpecFromConfig 把配置转换为 Spec。
//
// 对 map / slice 做浅拷贝，避免运行期误改配置（配置被多个适配器共享）。
// p 为 nil 时返回零值 Spec，调用方需自行保证 ID/SearchURL 非空。
func SpecFromConfig(p *config.Platform) Spec {
	if p == nil {
		return Spec{}
	}
	return Spec{
		ID:               p.ID,
		Name:             p.Name,
		Domain:           p.Domain,
		Experimental:     p.Experimental,
		SearchURL:        p.SearchURL,
		QueryParams:      copyMap(p.QueryParams),
		JobURLTemplate:   p.JobURLTemplate,
		Referer:          p.Referer,
		Headers:          copyMap(p.Headers),
		CookieEnv:        p.CookieEnv,
		ListPath:         p.ListPath,
		HasMorePath:      p.HasMorePath,
		ActiveOnlinePath: p.ActiveOnlinePath,
		ActiveTimePath:   p.ActiveTimePath,
		CodeField:        p.CodeField,
		MessageField:     p.MessageField,
		SuccessCodes:     copySlice(p.SuccessCodes),
		ErrorCodes:       copyMap(p.ErrorCodes),
		FieldMap:         copyFieldMap(p.FieldMap),
		PublishTimePaths: copySlice(p.PublishTimePaths),
		Cities:           copyMap(p.Cities),
		PageSize:         p.PageSize,
		MaxPages:         p.MaxPages,
	}
}

// copyMap 复制字符串 map，nil 输入返回 nil（保持「未配置」语义）。
func copyMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copySlice 复制字符串切片，nil 输入返回 nil。
func copySlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// copyFieldMap 复制「字段 -> 候选路径」两级结构，避免共享底层数组。
func copyFieldMap(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = copySlice(v)
	}
	return out
}
