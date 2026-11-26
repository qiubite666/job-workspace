// Package liepin 是猎聘（liepin.com）的适配器骨架。
//
// 它存在的意义是「样板 + 防线」，而不是能用的采集器：
//   - 配置里 enabled=false、experimental=true，编排层默认不会启动它；
//   - 即使被强行启用，FetchList 也会明确返回 BAD_CONFIG，而不是发一堆注定失败的请求后
//     假装“采到 0 条”——真实采集（登录态、反爬、HTML 列表解析）尚未实现。
//
// 接入真实实现时只需两步：在 platforms.json 补齐 queryParams/listPath/fieldMap，
// 删除本文件的 FetchList/Capabilities 覆盖（直接用 httpjson 默认行为），并按需补 PostMap。
//
// 注册方式与 boss 一致：由 main.go 调用 liepin.RegisterFactory()。
package liepin

import (
	"context"
	"strings"

	"jobworkspace/internal/config"
	"jobworkspace/internal/errs"
	"jobworkspace/internal/model"
	"jobworkspace/internal/platform"
	"jobworkspace/internal/platform/catalog"
	"jobworkspace/internal/platform/httpjson"
)

// 默认元信息：配置缺失时的兜底。
const (
	defaultName   = "猎聘"
	defaultDomain = "liepin.com"
)

// adapter 在通用引擎外面包一层，用于把「未实现」显式暴露出来。
type adapter struct {
	*httpjson.Adapter
}

// New 依据配置构造猎聘适配器（骨架）。
func New(p *config.Platform) platform.Adapter {
	spec := httpjson.SpecFromConfig(p)
	if strings.TrimSpace(spec.ID) == "" {
		spec.ID = "liepin"
	}
	if strings.TrimSpace(spec.Name) == "" {
		spec.Name = defaultName
	}
	if strings.TrimSpace(spec.Domain) == "" {
		spec.Domain = defaultDomain
	}
	// 骨架阶段一律标记为试验性，避免前端把它当成可用平台。
	spec.Experimental = true

	return &adapter{Adapter: httpjson.New(spec)}
}

// RegisterFactory 把猎聘工厂注册进 catalog；由 main.go 显式调用。
func RegisterFactory() {
	catalog.RegisterFactory("liepin", New)
}

// Capabilities 声明能力：列表采集尚未实现，因此 List=false。
func (a *adapter) Capabilities() platform.Capabilities {
	return platform.Capabilities{List: false, Detail: false}
}

// FetchList 明确拒绝采集。
//
// 不返回「空列表 + nil」，因为那会让编排层以为“这个平台今天没岗位”，
// 反而掩盖了适配器未实现的事实。
func (a *adapter) FetchList(_ context.Context, _ *platform.Runtime, _ model.Params) (model.ListResult, error) {
	e := errs.New(errs.CodeBadConfig, "猎聘适配器尚未实现真实采集（仅骨架），请勿在 live 模式启用")
	e.Platform = a.ID()
	return model.ListResult{}, e
}
