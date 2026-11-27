// Package model 定义全工程共享的数据结构。
//
// 这里是「跨平台统一数据契约」的唯一落点：所有招聘平台无论原始字段叫什么，
// 最终都必须收敛成 Job。新增平台时**不允许**修改本包，只能在适配器里做映射。
package model

// Job 是统一岗位模型，也是落盘与前端展示的唯一结构。
//
// 约定：
//   - 字符串字段缺值一律为空串 ""，不用 nil 悬挂；
//   - 数值字段缺值一律为 null（指针），前端据此展示「薪资未标注」；
//   - 切片字段缺值一律为空数组 []。
type Job struct {
	ID               string `json:"id"`               // 全局唯一：`${platform}:${externalId}` 或 `${platform}:${sha1 前16位}`
	Platform         string `json:"platform"`         // 平台标识，如 boss
	PlatformName     string `json:"platformName"`     // 平台中文名，如 BOSS直聘
	ExternalID       string `json:"externalId"`       // 平台原始 ID，可为空
	JobName          string `json:"jobName"`          // 职位名称
	BrandName        string `json:"brandName"`        // 公司名称
	CityName         string `json:"cityName"`         // 城市
	AreaDistrict     string `json:"areaDistrict"`     // 行政区
	BusinessDistrict string `json:"businessDistrict"` // 商圈
	Location         string `json:"location"`         // 展示用组合地点：城市·行政区·商圈

	SalaryDesc       string `json:"salaryDesc"`       // 薪资原文，永不改写
	SalaryMin        *int   `json:"salaryMin"`        // 原始单位下界（元）
	SalaryMax        *int   `json:"salaryMax"`        // 原始单位上界（元）
	SalaryMonths     *int   `json:"salaryMonths"`     // 年薪月数，如 14
	SalaryUnit       string `json:"salaryUnit"`       // month | day | year | ""
	SalaryKind       string `json:"salaryKind"`       // range | single | negotiable | unknown
	SalaryMonthlyMin *int   `json:"salaryMonthlyMin"` // 归一到元/月，用于排序比较
	SalaryMonthlyMax *int   `json:"salaryMonthlyMax"`
	SalaryText       string `json:"salaryText"` // 展示文本

	Experience     string   `json:"experience"`     // 经验要求
	Degree         string   `json:"degree"`         // 学历要求
	JobLabels      []string `json:"jobLabels"`      // 平台原始标签
	Skills         []string `json:"skills"`         // 技能关键词
	WelfareList    []string `json:"welfareList"`    // 福利标签
	BossName       string   `json:"bossName"`       // 招聘者
	BossTitle      string   `json:"bossTitle"`      // 招聘者职位
	ActiveTimeDesc string   `json:"activeTimeDesc"` // 活跃度描述
	// Description 是职位描述正文，纯文本（已去标签）。列表接口不返回，
	// 只有在 BOSS 打开详情页后由扩展回填。
	Description string `json:"description"`

	PublishedAt       string `json:"publishedAt"`       // RFC3339；无则为 ""
	PublishedAtSource string `json:"publishedAtSource"` // api | detail | missing
	JobURL            string `json:"jobUrl"`            // 职位详情链接
	// SecurityID 是 BOSS 详情页的访问凭据，缺了它打开链接会落到空页面，
	// 页面不会请求详情接口，扩展也就抓不到 JD。
	SecurityID string `json:"securityId"`

	BrandIndustry  string `json:"brandIndustry"`  // 公司行业
	BrandScaleName string `json:"brandScaleName"` // 公司规模
	BrandStageName string `json:"brandStageName"` // 融资阶段
	CollectedAt    string `json:"collectedAt"`    // 采集时间 RFC3339
	Keyword        string `json:"keyword"`        // 采集关键词
	QueryCity      string `json:"queryCity"`      // 采集城市
}

// JobDraft 是适配器交给映射层的中间态。
//
// 平台模块只负责「把原始条目扁平化到这里」，不做薪资解析、时间解析、指纹计算，
// 这些统一由 mapping.Finalize 完成，保证各平台解析口径一致。
type JobDraft struct {
	ExternalID       string
	SecurityID       string
	JobName          string
	BrandName        string
	CityName         string
	AreaDistrict     string
	BusinessDistrict string
	SalaryDesc       string
	Experience       string
	Degree           string
	JobLabels        []string
	Skills           []string
	WelfareList      []string
	BossName         string
	BossTitle        string
	ActiveTimeDesc   string
	// Description 是职位描述正文（详情接口才有，列表接口不返回）。
	Description    string
	JobURL         string
	BrandIndustry  string
	BrandScaleName string
	BrandStageName string
	// PublishedRaw 是「已取到值」的发布时间候选，按优先级排列，交给 parser.ParsePublishTime。
	// 支持时间戳字符串、"2026-11-24"、"3天前" 等格式。
	PublishedRaw []string
	Extra        map[string]any
}

// Params 是一次采集的输入参数。
//
// 采集只有联网一条路径，因此不再携带模式与本地文件字段。
type Params struct {
	Query           string   `json:"query"`
	City            string   `json:"city"`
	Pages           int      `json:"pages"`
	PageSize        int      `json:"pageSize"`
	Platforms       []string `json:"platforms,omitempty"` // 限定平台，空=全部启用
	Cookie          string   `json:"-"`                   // 不下发到前端、不落盘
	AllowDisallowed bool     `json:"-"`                   // 显式放行 robots 禁止路径
}

// ListResult 是适配器 fetchList 的产出。
type ListResult struct {
	Items   []map[string]any // 原始条目（平台 JSON 对象）
	HasMore bool             // 是否还有下一页
}

// Attempt 记录一次降级链尝试，用于可观测性。
type Attempt struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	MS      int64  `json:"ms"`
}

// ErrItem 是单条采集错误。
type ErrItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Page    int    `json:"page,omitempty"`
}

// PlatformStat 是单平台采集统计。
type PlatformStat struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Status   string    `json:"status"` // ok | degraded | failed | skipped
	UsedStep string    `json:"usedStep,omitempty"`
	Pages    int       `json:"pages"`
	Fetched  int       `json:"fetched"`
	Mapped   int       `json:"mapped"`
	Dropped  int       `json:"dropped"`
	Attempts []Attempt `json:"attempts"`
	Errors   []ErrItem `json:"errors"`
}

// Totals 是全局汇总。
type Totals struct {
	Platforms  int `json:"platforms"`
	Fetched    int `json:"fetched"`
	Mapped     int `json:"mapped"`
	Dropped    int `json:"dropped"`
	Unique     int `json:"unique"`
	Duplicates int `json:"duplicates"`
}

// Report 是一次采集的可观测报告，落盘为 data/collect.report.json。
type Report struct {
	OK         bool           `json:"ok"`
	StartedAt  string         `json:"startedAt"`
	FinishedAt string         `json:"finishedAt"`
	DurationMS int64          `json:"durationMs"`
	Params     Params         `json:"params"`
	Platforms  []PlatformStat `json:"platforms"`
	Totals     Totals         `json:"totals"`
}

// CollectResult 是 pipeline 的输出。
type CollectResult struct {
	Jobs   []Job
	Report Report
}

// FinalizeContext 提供收口时需要的上下文。
type FinalizeContext struct {
	Platform     string
	PlatformName string
	Keyword      string
	QueryCity    string
	CollectedAt  string
}
