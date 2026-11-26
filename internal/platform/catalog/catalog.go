// Package catalog 是「平台工厂注册表 + 按配置构建适配器」的装配层。
//
// # 为什么需要它
//
// platform.Register 需要适配器**实例**，而实例需要配置（URL、字段映射、城市码都在配置里）。
// 所以注册被拆成两步：
//  1. 各平台包提供 `New(p *config.Platform) platform.Adapter` 与 `RegisterFactory()`；
//  2. 启动时按加载好的配置，用工厂造实例并 platform.Register。
//
// # 导入约定（重要）
//
// 本包的 init() **不**导入 boss/liepin 等子包：子包需要调用 catalog.RegisterFactory，
// 若 catalog 反向导入子包就形成循环依赖。因此约定为：
//
//	// main.go
//	import (
//	    "jobworkspace/internal/platform/boss"
//	    "jobworkspace/internal/platform/catalog"
//	    "jobworkspace/internal/platform/liepin"
//	)
//
//	func init() {
//	    boss.RegisterFactory()
//	    liepin.RegisterFactory()
//	}
//	// 之后在命令执行处调用：adapters := catalog.Build(cfg)
//
// 也就是说：**子包负责实现，main 负责登记，catalog 负责存工厂与装配**。
// 空导入（`_`）无法调用 RegisterFactory，因此必须是具名导入 + 显式调用；
// 这里的「必须导入」约定即：新增平台必须同时在 main 里加一行 import 与一行 RegisterFactory()，
// 否则 catalog.Build 会因找不到工厂而跳过该平台并打印 warn。
package catalog

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"jobworkspace/internal/config"
	"jobworkspace/internal/platform"
)

// Factory 是「配置 -> 适配器实例」的构造函数。
type Factory func(p *config.Platform) platform.Adapter

var (
	mu        sync.RWMutex
	factories = make(map[string]Factory)
)

// RegisterFactory 登记某平台的工厂。
//
// 同一 ID 重复登记视为「后者覆盖前者」（便于测试替换），不会 panic；
// 真正的重复注册保护在 platform.Register（会 panic，因为它意味着两个实例争用同一 ID）。
func RegisterFactory(id string, f Factory) {
	id = strings.TrimSpace(id)
	if id == "" || f == nil {
		// 开发期错误：静默丢弃会让人以为注册成功了，这里打 warn 暴露出来。
		warnf("catalog: RegisterFactory 参数非法（id=%q, factory=%v），已忽略", id, f == nil)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	factories[id] = f
}

// New 用指定 ID 的工厂构造适配器；工厂未登记返回 nil（由调用方决定是否跳过）。
func New(id string, p *config.Platform) platform.Adapter {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	mu.RLock()
	f := factories[id]
	mu.RUnlock()
	if f == nil {
		return nil
	}
	return f(p)
}

// Build 按配置顺序构建全部适配器，并完成 platform.Register。
//
// 行为：
//   - 遍历 cfg.Ordered()（含 disabled 平台，是否启用由编排层判断）；
//   - 找不到工厂 → 跳过并 warn，不 panic；
//   - 已注册过的 ID（重复调用 Build，或测试里预先注册过）→ 直接复用，保证幂等。
func Build(cfg *config.Config) []platform.Adapter {
	if cfg == nil {
		return nil
	}

	ordered := cfg.Ordered()
	out := make([]platform.Adapter, 0, len(ordered))

	for _, p := range ordered {
		if p == nil || strings.TrimSpace(p.ID) == "" {
			continue
		}

		// 幂等：已注册则复用，避免 platform.Register 的重复注册 panic。
		if existing, ok := platform.Get(p.ID); ok {
			out = append(out, existing)
			continue
		}

		adapter := New(p.ID, p)
		if adapter == nil {
			warnf("catalog: 未找到平台 %q 的工厂，已跳过（请在 main 中调用 %s.RegisterFactory()）", p.ID, p.ID)
			continue
		}

		platform.Register(adapter)
		out = append(out, adapter)
	}

	return out
}

// Factories 返回已登记的平台 ID 列表，便于 doctor 子命令自检。
func Factories() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for id := range factories {
		out = append(out, id)
	}
	return out
}

// warnf 向 stderr 打印告警：装配期的信息不应污染 JSON 输出（stdout）。
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[warn] "+format+"\n", args...)
}
