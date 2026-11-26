// Package template 是「新增平台」的复制即用骨架。
//
// 本包**不被任何其他包引用**，因此改坏也不影响主流程；它的价值是把「新增平台要动哪几处」固定在注释里。
//
// # 新增平台 3 步
//
//  1. 复制目录：`cp -r internal/platform/template internal/platform/<id>`，
//     把 package 名改成 <id>，删掉本段说明。
//  2. 实现四个方法：ID / Meta / FetchList / MapItem（NormalizeParams 与 Capabilities 有可用默认实现）。
//     若平台是「GET + JSON 列表 + 配置化字段」这种最常见形态，**不要自己写请求代码**，
//     直接在 New 里返回 `httpjson.New(spec)`，只补 Spec.PostMap 即可（参考 internal/platform/boss）。
//  3. 增加配置 + 登记：在 internal/config/platforms.json 的 platforms 下加一段（searchUrl/listPath/fieldMap…），
//     再在 main.go 里 `import "jobworkspace/internal/platform/<id>"` 并调用 `<id>.RegisterFactory()`
//     （catalog 不空导入子包，详见 internal/platform/catalog 的包注释）。
//
// 最后跑 `go build ./... && go run . platforms` 确认平台已出现在列表里。
package template

import (
	"context"
	"strings"

	"jobworkspace/internal/config"
	"jobworkspace/internal/errs"
	"jobworkspace/internal/mapping"
	"jobworkspace/internal/model"
	"jobworkspace/internal/platform"
)

// 默认元信息：配置缺项时的兜底，避免前端/日志出现空白。
const (
	platformID  = "template"
	defaultName = "示例平台"
)

// Adapter 是骨架实现，只保存配置引用。
//
// TODO: 增加平台私有字段（如接口版本、签名密钥的环境变量名）时，
// 请确认它们都是只读的，否则并发采集会出问题。
type Adapter struct {
	cfg *config.Platform
}

// New 依据配置构造适配器。
//
// TODO: 若平台只是「GET + JSON 列表」，把这里换成：
//
//	spec := httpjson.SpecFromConfig(p)
//	spec.PostMap = postMap
//	return httpjson.New(spec)
//
// 这样整个包只需要保留一个 postMap 函数。
func New(p *config.Platform) platform.Adapter {
	return &Adapter{cfg: p}
}

// ID 返回平台标识，必须与配置里的 key 完全一致。
func (a *Adapter) ID() string {
	if a.cfg != nil && strings.TrimSpace(a.cfg.ID) != "" {
		return a.cfg.ID
	}
	return platformID
}

// Meta 返回平台元信息。
//
// TODO: 把 Name/Domain 换成平台真实值；Experimental 按成熟度赋值。
func (a *Adapter) Meta() platform.Meta {
	name, domain, experimental := defaultName, "", false
	if a.cfg != nil {
		if s := strings.TrimSpace(a.cfg.Name); s != "" {
			name = s
		}
		domain = a.cfg.Domain
		experimental = a.cfg.Experimental
	}
	return platform.Meta{Name: name, Domain: domain, Experimental: experimental}
}

// Capabilities 声明能力；只有实现了详情补全才把 Detail 置 true。
func (a *Adapter) Capabilities() platform.Capabilities {
	return platform.Capabilities{List: true, Detail: false}
}

// NormalizeParams 做参数兜底。
//
// 约定：p.Pages 表示**当前页号**（1-based），由编排层逐页递推。
// TODO: 若平台用 0-based 页码或游标翻页，在这里换算（游标建议放进 p 的副本或适配器外置状态，慎用）。
func (a *Adapter) NormalizeParams(p model.Params) model.Params {
	if p.Pages < 1 {
		p.Pages = 1
	}
	pageSize := 20 // TODO: 换成平台默认页大小（或读 a.cfg.PageSize）
	if p.PageSize < 1 {
		p.PageSize = pageSize
	}
	if p.PageSize > 100 {
		p.PageSize = 100
	}
	return p
}

// FetchList 拉取一页列表。
//
// 返回值约定：
//   - Items 为空且 err 为 nil → 本页无数据，编排层停止翻页；
//   - err 必须是 *errs.PlatformError（用 errs.New/errs.Wrap，并设置 Platform 字段），
//     编排层据此判断是否重试 / 降级 / 停止。
func (a *Adapter) FetchList(_ context.Context, _ *platform.Runtime, _ model.Params) (model.ListResult, error) {
	// TODO: 在这里完成「拼 URL → rt.Client.GetJSON → 校验业务码 → 取列表」。
	//       提示：rt.Client 已装配好限速、重试、robots 与 cookie，直接用，不要另建 http.Client。
	e := errs.New(errs.CodeBadConfig, "平台 %s 的 FetchList 尚未实现（这是 template 骨架）", a.ID())
	e.Platform = a.ID()
	return model.ListResult{}, e
}

// MapItem 把原始条目映射为统一草稿。
//
// 默认实现交给 mapping.BuildDraft（由配置里的 fieldMap 决定取值），这已经覆盖绝大多数平台。
func (a *Adapter) MapItem(_ *platform.Runtime, raw map[string]any) (model.JobDraft, error) {
	if raw == nil {
		return model.JobDraft{}, nil
	}
	spec := mapping.Spec{
		Platform:     a.ID(),
		PlatformName: a.Meta().Name,
	}
	// TODO: 从 a.cfg 拷入 FieldMap / PublishTimePaths / JobURLTemplate，
	//       或直接改用 httpjson.New（它已经处理好了这些）。
	return mapping.BuildDraft(raw, spec), nil
}

// 编译期接口断言：骨架必须始终满足 platform.Adapter，改接口时能第一时间发现。
var _ platform.Adapter = (*Adapter)(nil)
