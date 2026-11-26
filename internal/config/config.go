// Package config 负责加载与归一化「平台接入配置」。
//
// 配置是接入层唯一的数据来源：请求地址、参数模板、业务码映射、字段映射、城市码表都写在这里，
// 平台适配器只做「读配置 + 少量特有点修正」，核心流程（pipeline / store / web）不感知任何平台细节。
//
// 使用方式：
//
//	cfg, err := config.Load("")        // 空串 → 使用内置 platforms.json
//	cfg, err := config.Load("./my.json") // 非空 → 用户外部覆盖
//	for _, p := range cfg.Ordered() { ... }
//
// 约定：本包只读取文件与内存中的 JSON，不发起网络请求，不依赖其他业务包。
package config

import (
	"bytes"
	_ "embed" // 用于 //go:embed 内嵌默认配置
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"jobworkspace/internal/errs"
)

// builtinPlatforms 是编译进二进制的默认配置，保证「不带任何参数也能跑」。
//
//go:embed platforms.json
var builtinPlatforms []byte

// builtinSource 是内置配置在错误信息中的展示名。
const builtinSource = "内置 platforms.json"

// Defaults 是全局兜底参数，用于回填各平台未显式声明的字段。
type Defaults struct {
	Pages         int   `json:"pages"`         // 默认翻页数
	PageSize      int   `json:"pageSize"`      // 默认每页条数
	MinDelayMS    int   `json:"minDelayMs"`    // 请求最小间隔（毫秒）
	MaxDelayMS    int   `json:"maxDelayMs"`    // 请求最大间隔（毫秒）
	TimeoutMS     int   `json:"timeoutMs"`     // 单请求超时（毫秒）
	Retries       int   `json:"retries"`       // 可重试错误的重试次数
	RespectRobots *bool `json:"respectRobots"` // 是否遵守 robots.txt；nil 视为 true
}

// Platform 是单个平台的接入配置。
//
// JSON tag 是配置文件的外部契约，**不得随意改动**；新增字段必须同时提供零值可用的语义。
type Platform struct {
	ID               string              `json:"id"`               // 平台标识，为空时由 Load 用 map key 回填
	Enabled          bool                `json:"enabled"`          // 是否参与采集
	Priority         int                 `json:"priority"`         // 升序，越小越先尝试
	Experimental     bool                `json:"experimental"`     // 试验性适配器，前端提示用
	Name             string              `json:"name"`             // 中文名，如 BOSS直聘
	Domain           string              `json:"domain"`           // 主域名，如 zhipin.com
	SearchURL        string              `json:"searchUrl"`        // 列表接口地址
	JobURLTemplate   string              `json:"jobUrlTemplate"`   // 详情链接模板，支持 {{externalId}}
	Referer          string              `json:"referer"`          // 请求 Referer
	Headers          map[string]string   `json:"headers"`          // 额外请求头
	CookieEnv        string              `json:"cookieEnv"`        // 登录态环境变量名，如 BOSS_COOKIE
	QueryParams      map[string]string   `json:"queryParams"`      // 查询参数模板，值支持 {{query}} 等占位符
	ListPath         string              `json:"listPath"`         // 列表数组的点路径，空则期望顶层数组
	HasMorePath      string              `json:"hasMorePath"`      // 是否还有下一页的点路径
	CodeField        string              `json:"codeField"`        // 业务码字段点路径
	MessageField     string              `json:"messageField"`     // 业务消息字段点路径
	SuccessCodes     []string            `json:"successCodes"`     // 视为成功的业务码；空则只有 "0" 成功
	ErrorCodes       map[string]string   `json:"errorCodes"`       // 业务码 -> errs.Code 的显式映射
	FieldMap         map[string][]string `json:"fieldMap"`         // 统一字段 -> 候选路径
	PublishTimePaths []string            `json:"publishTimePaths"` // 发布时间候选路径
	Cities           map[string]string   `json:"cities"`           // 城市名 -> 城市码

	PageSize   int `json:"pageSize"`   // 每页条数
	MaxPages   int `json:"maxPages"`   // 最大翻页数，0 表示不限制
	MinDelayMS int `json:"minDelayMs"` // 请求最小间隔（毫秒）
	MaxDelayMS int `json:"maxDelayMs"` // 请求最大间隔（毫秒）
	TimeoutMS  int `json:"timeoutMs"`  // 单请求超时（毫秒）
	Retries    int `json:"retries"`    // 重试次数

	RespectRobots *bool `json:"respectRobots"` // 指针：nil 视为遵守 robots
	// AllowDisallowed 只是配置里的「默认建议」，真正的放行开关由 CLI 参数（--allow-disallowed）
	// 经 model.Params.AllowDisallowed 传给 httpx，适配器不自行读取本字段。
	AllowDisallowed bool `json:"allowDisallowed"`
}

// Config 是整份配置。
type Config struct {
	Version   int                  `json:"version"`
	Defaults  Defaults             `json:"defaults"`
	Platforms map[string]*Platform `json:"platforms"`
}

// Load 读取配置：path 为空时使用编译期内置的 platforms.json，否则读取外部文件。
//
// 返回的 Config 已完成归一化（ID 回填、Defaults 回填），可直接使用。
// JSON 语法错误会带出「第几行第几列」，字段类型错误会带出字段名。
func Load(path string) (*Config, error) {
	data, src := builtinPlatforms, builtinSource
	if p := strings.TrimSpace(path); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, errs.Wrap(errs.CodeBadConfig, err, "读取配置文件 %s 失败", p)
		}
		data, src = b, p
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&cfg); err != nil {
		return nil, describeJSONError(err, data, src)
	}
	// 只允许单个 JSON 文档，防止「两份配置拼接」这类误操作。
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, errs.New(errs.CodeBadConfig, "配置文件 %s 在首个 JSON 文档之后仍有多余内容", src)
	}

	if cfg.Version <= 0 {
		cfg.Version = 1
	}
	if len(cfg.Platforms) == 0 {
		return nil, errs.New(errs.CodeBadConfig, "配置文件 %s 未定义任何平台（platforms 为空）", src)
	}

	for key, p := range cfg.Platforms {
		if p == nil {
			return nil, errs.New(errs.CodeBadConfig, "配置文件 %s 中平台 %q 为 null", src, key)
		}
		if p.ID == "" {
			p.ID = key
		}
		applyDefaults(p, cfg.Defaults)
	}

	return &cfg, nil
}

// Platform 按 ID 取平台配置；未配置返回 nil（由调用方决定跳过，不凭空造默认平台）。
func (c *Config) Platform(id string) *Platform {
	if c == nil {
		return nil
	}
	if p, ok := c.Platforms[id]; ok {
		return p
	}
	return nil
}

// Ordered 返回全部平台配置（含 disabled），按 Priority 升序、同优先级按 ID 字典序。
//
// 是否启用由调用方读 Enabled 判断；这里不做过滤，避免 web 无法展示被禁用的平台。
func (c *Config) Ordered() []*Platform {
	if c == nil || len(c.Platforms) == 0 {
		return nil
	}
	out := make([]*Platform, 0, len(c.Platforms))
	for _, p := range c.Platforms {
		if p != nil {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DelayRange 返回请求间隔区间；配置异常（负数、上界小于下界）会被规范化，不会返回非法区间。
func (p *Platform) DelayRange() (time.Duration, time.Duration) {
	if p == nil {
		return 0, 0
	}
	minMS, maxMS := p.MinDelayMS, p.MaxDelayMS
	if minMS < 0 {
		minMS = 0
	}
	if maxMS < 0 {
		maxMS = 0
	}
	if maxMS < minMS {
		maxMS = minMS
	}
	return time.Duration(minMS) * time.Millisecond, time.Duration(maxMS) * time.Millisecond
}

// RobotsEnabled 返回是否遵守 robots.txt；未显式配置（nil）时默认遵守。
func (p *Platform) RobotsEnabled() bool {
	if p == nil || p.RespectRobots == nil {
		return true
	}
	return *p.RespectRobots
}

// ---------- 内部辅助 ----------

// applyDefaults 用全局 Defaults 回填平台里为零值的字段。
//
// 只回填「零值」，因此平台可以显式写 retries:0 之外的值；Retries 为 0 会被回填为默认值，
// 若确实需要「不重试」请在 defaults 里配 0。
func applyDefaults(p *Platform, d Defaults) {
	if p.PageSize <= 0 {
		p.PageSize = d.PageSize
	}
	if p.MinDelayMS <= 0 {
		p.MinDelayMS = d.MinDelayMS
	}
	if p.MaxDelayMS <= 0 {
		p.MaxDelayMS = d.MaxDelayMS
	}
	if p.TimeoutMS <= 0 {
		p.TimeoutMS = d.TimeoutMS
	}
	if p.Retries <= 0 {
		p.Retries = d.Retries
	}
	// robots 开关必须回填，否则 defaults.respectRobots=false 会被静默忽略。
	// 用指针承接是为了区分「未配置」与「显式 false」：未配置时保持 nil，
	// 由 RobotsEnabled() 回落为 true（保守优先，绝不因漏配而放开限制）。
	if p.RespectRobots == nil && d.RespectRobots != nil {
		p.RespectRobots = d.RespectRobots
	}
}

// describeJSONError 把 encoding/json 的错误翻译成带位置与字段名的中文错误。
func describeJSONError(err error, data []byte, src string) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError

	switch {
	case errors.As(err, &syntaxErr):
		line, col := lineCol(data, syntaxErr.Offset)
		return errs.Wrap(errs.CodeBadConfig, err, "配置文件 %s 第 %d 行第 %d 列存在 JSON 语法错误", src, line, col)
	case errors.As(err, &typeErr):
		line, col := lineCol(data, typeErr.Offset)
		field := typeErr.Field
		if field == "" {
			field = "<顶层>"
		}
		return errs.Wrap(errs.CodeBadConfig, err, "配置文件 %s 第 %d 行第 %d 列字段 %s 类型不匹配（期望 %s）", src, line, col, field, typeErr.Type)
	default:
		return errs.Wrap(errs.CodeBadConfig, err, "解析配置文件 %s 失败", src)
	}
}

// lineCol 把字节偏移换算成 1-based 的行列号。
func lineCol(data []byte, offset int64) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	line, col := 1, 1
	for i := int64(0); i < offset; i++ {
		if data[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}
