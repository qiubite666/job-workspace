// Package platform 定义招聘平台的统一接入接口与注册表。
//
// 这是整个接入层的**唯一扩展点**：
//   - 新增平台：在 internal/platform/<id>/ 下实现 Adapter 并 Register，再在 catalog 中空导入；
//   - 核心流程（pipeline / store / web）只依赖本接口，永不感知具体平台。
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"jobworkspace/internal/config"
	"jobworkspace/internal/httpx"
	"jobworkspace/internal/model"
)

// Meta 是平台元信息，用于前端展示与日志。
type Meta struct {
	Name         string // 中文名，如「BOSS直聘」
	Domain       string // 主域名，如 zhipin.com
	Experimental bool   // 是否为试验性适配器
}

// Capabilities 声明平台能力，供编排层决定是否发起详情请求。
type Capabilities struct {
	List   bool // 支持列表采集
	Detail bool // 支持详情补全
}

// Runtime 是框架注入给适配器的运行时依赖（适配器只读）。
type Runtime struct {
	Client *httpx.Client     // 已装配限速/重试/robots 的 HTTP 客户端
	Config *config.Platform  // 该平台合并后的配置
	Log    *slog.Logger      // 结构化日志
	Now    func() time.Time  // 可注入时钟，便于测试
	Cities map[string]string // 城市名 -> 城市码
}

// Adapter 是招聘平台适配器的统一契约。
//
// 实现方只需关心四件事：元信息、参数归一、拉列表、字段映射。
// 薪资解析、时间解析、去重、排序、落盘全部由核心完成。
type Adapter interface {
	// ID 返回平台标识，必须与配置中的 key 一致（小写字母/数字/连字符）。
	ID() string
	// Meta 返回平台元信息。
	Meta() Meta
	// Capabilities 返回平台能力。
	Capabilities() Capabilities
	// NormalizeParams 做平台特有的参数归一（如城市名转城市码）。
	// 允许原地修改并返回；不得返回 error 中断采集。
	NormalizeParams(p model.Params) model.Params
	// FetchList 拉取一页岗位列表。
	// 返回 items 为空且 err 为 nil 时视为「本页无数据」，编排层停止翻页。
	FetchList(ctx context.Context, rt *Runtime, p model.Params) (model.ListResult, error)
	// MapItem 把平台原始条目映射为统一草稿。
	MapItem(rt *Runtime, raw map[string]any) (model.JobDraft, error)
}

var (
	mu       sync.RWMutex
	registry = make(map[string]Adapter)
	order    []string
)

// Register 由各平台包的 init() 调用，完成自注册。
//
// 重复注册同一 ID 会 panic —— 这属于开发期错误，应在启动时立刻暴露，
// 而不是静默覆盖导致采集到错误平台的数据。
func Register(a Adapter) {
	if a == nil {
		panic("platform: Register(nil)")
	}
	id := a.ID()
	if id == "" {
		panic("platform: adapter ID 不能为空")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[id]; dup {
		panic(fmt.Sprintf("platform: 适配器 %q 重复注册", id))
	}
	registry[id] = a
	order = append(order, id)
}

// All 返回已注册适配器，按配置优先级排序（未配置的排最后）。
//
// 注意：先在锁内完成快照再在锁外排序。RWMutex 的 RLock 不可重入——
// 若持读锁时再次 RLock，且此刻有写者排队，会直接死锁。
func All() []Adapter {
	mu.RLock()
	out := make([]Adapter, 0, len(order))
	for _, id := range order {
		out = append(out, registry[id])
	}
	fn := priorityFunc
	mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := resolvePriority(fn, out[i].ID()), resolvePriority(fn, out[j].ID())
		if pi != pj {
			return pi < pj
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// resolvePriority 在无锁状态下解析优先级，避免锁重入。
func resolvePriority(fn func(string) int, id string) int {
	if fn == nil {
		return 100
	}
	return fn(id)
}

// Get 按 ID 取适配器。
func Get(id string) (Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[id]
	return a, ok
}

// IDs 返回全部已注册平台标识（注册顺序）。
func IDs() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// priorityFunc 由 config 包在加载后注入，避免 platform -> config 的反向依赖耦合。
var priorityFunc func(id string) int

// SetPriorityResolver 注入优先级解析函数（由 main 在加载配置后调用）。
func SetPriorityResolver(fn func(id string) int) {
	mu.Lock()
	defer mu.Unlock()
	priorityFunc = fn
}
