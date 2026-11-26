// Package boss 是 BOSS直聘（zhipin.com）的适配器。
//
// 绝大部分行为由配置驱动（internal/config/platforms.json 的 boss 段），这里只补三件平台特有的小事：
//   - jobLabels 里混着经验/学历标签，字段缺失时用来回填；
//   - bossOnline 为 true 但缺活跃度文案时回填「在线」；
//   - 元信息缺失时给出安全默认值。
//
// 注册方式：平台实例依赖配置，因此不能在本包 init 里注册。
// 由 main.go 调用 boss.RegisterFactory() 把工厂交给 catalog，再由 catalog.Build(cfg) 构建。
package boss

import (
	"strings"

	"jobworkspace/internal/config"
	"jobworkspace/internal/model"
	"jobworkspace/internal/platform"
	"jobworkspace/internal/platform/catalog"
	"jobworkspace/internal/platform/httpjson"
)

// 默认元信息：配置缺失时的兜底，保证前端与日志不出现空白。
const (
	defaultName   = "BOSS直聘"
	defaultDomain = "zhipin.com"
)

// New 依据配置构造 BOSS 适配器。
func New(p *config.Platform) platform.Adapter {
	spec := httpjson.SpecFromConfig(p)
	if strings.TrimSpace(spec.ID) == "" {
		spec.ID = "boss"
	}
	if strings.TrimSpace(spec.Name) == "" {
		spec.Name = defaultName
	}
	if strings.TrimSpace(spec.Domain) == "" {
		spec.Domain = defaultDomain
	}
	spec.PostMap = postMap
	return httpjson.New(spec)
}

// RegisterFactory 把 BOSS 工厂注册进 catalog；由 main.go 显式调用（不在 init 中调用，避免包间循环依赖）。
func RegisterFactory() {
	catalog.RegisterFactory("boss", New)
}

// postMap 修正 BOSS 特有的字段缺失。
//
// 只填空值，不覆盖已有值：配置里的 FieldMap 才是权威来源。
func postMap(_ *platform.Runtime, draft *model.JobDraft, raw map[string]any) {
	if draft == nil {
		return
	}

	if strings.TrimSpace(draft.Experience) == "" {
		draft.Experience = pickExperienceLabel(draft.JobLabels)
	}
	if strings.TrimSpace(draft.Degree) == "" {
		draft.Degree = pickDegreeLabel(draft.JobLabels)
	}
	if strings.TrimSpace(draft.ActiveTimeDesc) == "" && isTrue(raw["bossOnline"]) {
		draft.ActiveTimeDesc = "在线"
	}
}

// pickExperienceLabel 从 jobLabels 里挑经验标签（如「3-5年」「经验不限」「应届生」）。
func pickExperienceLabel(labels []string) string {
	for _, raw := range labels {
		label := strings.TrimSpace(raw)
		if label == "" {
			continue
		}
		switch {
		case strings.Contains(label, "经验不限"), label == "不限":
			return label
		case strings.Contains(label, "应届"), strings.Contains(label, "在校"):
			return label
		case strings.Contains(label, "年"):
			// 「3-5年」「1年以内」「10年以上」等；
			// 学历标签里不会出现「年」，因此这里不会误判。
			return label
		}
	}
	return ""
}

// pickDegreeLabel 从 jobLabels 里挑学历标签（大专/本科/硕士/博士/学历不限 等）。
func pickDegreeLabel(labels []string) string {
	keywords := []string{"学历不限", "大专", "本科", "硕士", "博士", "MBA", "EMBA", "中专", "高中"}
	for _, raw := range labels {
		label := strings.TrimSpace(raw)
		if label == "" {
			continue
		}
		for _, kw := range keywords {
			if strings.Contains(label, kw) {
				return label
			}
		}
	}
	return ""
}

// isTrue 兼容 bool 与字符串形态的布尔值（BOSS 不同接口返回形态不一致）。
func isTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "true" || s == "1" || s == "yes"
	case float64:
		return x != 0
	default:
		return false
	}
}
