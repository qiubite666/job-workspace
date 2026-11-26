// Package parser 提供「薪资原文」与「发布时间文本」的纯函数解析能力。
//
// 本包是全工程最底层：只依赖标准库，不 import 任何 internal 包，
// 因此可以被 mapping / pipeline / web 任意层安全引用而不产生循环依赖。
//
// 设计原则：
//   - 解析失败一律返回零值结构（Kind=unknown），绝不 panic、绝不返回负数；
//   - 保留「原文语义」的同时，额外给出归一到「元/月」的区间供排序比较使用。
package parser

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Salary.Kind / Salary.Unit 的取值常量，便于调用方做分支判断。
const (
	KindRange      = "range"      // 明确区间，如 20-35K
	KindSingle     = "single"     // 单一数值，如 25K / 20K以上
	KindNegotiable = "negotiable" // 面议
	KindUnknown    = "unknown"    // 无法识别

	UnitMonth = "month" // 元/月（默认）
	UnitDay   = "day"   // 元/天
	UnitYear  = "year"  // 元/年
)

// Salary 是薪资文本的结构化解析结果。
//
// Min/Max 为「原始单位」下的整数金额（元），单值解析时 Max 可能为 nil
// （`20K以上` 这类只给出下界的写法）；Months 为年薪月数（如 14），未标注为 nil。
// Unit 取 month/day/year/""；Kind 取 range/single/negotiable/unknown。
// MonthlyMin/MonthlyMax 已归一到「元/月」，用于跨岗位排序；
// 对于 `20K以上` 这种只有下界的情况，两个月薪字段都会给出同一个值。
type Salary struct {
	Min, Max               *int
	Months                 *int
	Unit                   string
	Kind                   string
	MonthlyMin, MonthlyMax *int
}

var (
	// 数字 + 可选单位后缀：20 / 35K / 1.5万 / 8000
	reNumberUnit = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(k|千|万|w)?`)
	// 阿拉伯数字月数：14薪 / 13 薪
	reMonthsNum = regexp.MustCompile(`\d{1,2}\s*薪`)
	// 中文数字月数：十三薪 / 二十四薪
	reMonthsCN = regexp.MustCompile(`[一二三四五六七八九十]{1,3}\s*薪`)
)

// 全角 → 半角：数字、单位字母、连接符与常见标点。
var salaryNormalizer = strings.NewReplacer(
	"０", "0", "１", "1", "２", "2", "３", "3", "４", "4",
	"５", "5", "６", "6", "７", "7", "８", "8", "９", "9",
	"ｋ", "k", "Ｋ", "K", "ｗ", "w", "Ｗ", "W",
	"－", "-", "～", "-", "—", "-", "–", "-", "−", "-", "∼", "-", "~", "-",
	"至", "-", "到", "-",
	"．", ".", "。", ".", "，", ",", "／", "/",
	"（", "(", "）", ")", "：", ":",
	"　", " ",
)

// ParseSalary 把薪资原文解析成结构化结果。
//
// 支持的写法包括 `20-35K·14薪`、`30K-60K`、`1-1.5万`、`8000-12000`、
// `20K以上`、`25K`、`300-500元/天`、`30-50万/年`、`面议`、`薪资保密` 等。
//
// 单位换算规则：K/k → ×1000；万/w → ×10000；元/天 → ×21.75 折算月薪；
// 元/年 → ÷12 折算月薪；裸数字默认「元/月」。
// 无法识别出任何数字时返回 Kind=unknown 且所有数值字段为 nil；
// 识别到「面议 / Negotiable」时返回 Kind=negotiable。
// 若原文 Min > Max（脏数据），内部会自动交换，保证 Min <= Max。
func ParseSalary(text string) Salary {
	out := Salary{Kind: KindUnknown}
	raw := strings.TrimSpace(text)
	if raw == "" {
		return out
	}

	norm := salaryNormalizer.Replace(raw)
	if strings.Contains(norm, "面议") || strings.Contains(strings.ToLower(norm), "negotiable") {
		out.Kind = KindNegotiable
		return out
	}

	// 月数（·14薪 / ×13薪 / 十三薪）需要先从文本中摘除，
	// 否则 14 会被误当成薪资数值参与区间计算。
	months := findMonths(norm)
	core := stripMonths(norm)
	core = strings.ReplaceAll(core, "以上", "")
	core = strings.ReplaceAll(core, "起", "")
	hasLowerBound := strings.Contains(norm, "以上") || strings.Contains(norm, "起")

	matches := reNumberUnit.FindAllStringSubmatch(core, -1)
	if len(matches) == 0 {
		return out // unknown，不 panic
	}

	type amount struct {
		value    float64
		mult     float64
		explicit bool
	}
	nums := make([]amount, 0, 2)
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			continue
		}
		a := amount{value: v, mult: 1}
		switch strings.ToLower(m[2]) {
		case "k", "千":
			a.mult, a.explicit = 1000, true
		case "万", "w":
			a.mult, a.explicit = 10000, true
		}
		nums = append(nums, a)
		if len(nums) == 2 {
			break
		}
	}
	if len(nums) == 0 {
		return out
	}

	// 单位只写在区间右侧（`1-1.5万`、`20-35K`）时，左侧继承右侧单位。
	if len(nums) == 2 {
		if !nums[0].explicit && nums[1].explicit {
			nums[0].mult = nums[1].mult
		}
		if !nums[1].explicit && nums[0].explicit {
			nums[1].mult = nums[0].mult
		}
	}

	unit := detectUnit(core)
	toYuan := func(a amount) int { return int(math.Round(a.value * a.mult)) }
	toMonthly := func(v int) int {
		switch unit {
		case UnitDay:
			return int(math.Round(float64(v) * 21.75))
		case UnitYear:
			return int(math.Round(float64(v) / 12))
		default:
			return v
		}
	}

	if len(nums) == 1 {
		min := toYuan(nums[0])
		if min < 0 {
			min = 0
		}
		out.Min = &min
		out.Unit = unit
		out.Kind = KindSingle
		out.Months = months
		monthly := toMonthly(min)
		out.MonthlyMin, out.MonthlyMax = &monthly, &monthly
		if !hasLowerBound {
			max := min
			out.Max = &max
		}
		return out
	}

	lo, hi := toYuan(nums[0]), toYuan(nums[1])
	if lo < 0 {
		lo = 0
	}
	if hi < 0 {
		hi = 0
	}
	if lo > hi { // 容错：脏数据 Min > Max
		lo, hi = hi, lo
	}
	out.Min, out.Max = &lo, &hi
	out.Unit = unit
	out.Kind = KindRange
	out.Months = months

	mLo, mHi := toMonthly(lo), toMonthly(hi)
	if mLo > mHi {
		mLo, mHi = mHi, mLo
	}
	out.MonthlyMin, out.MonthlyMax = &mLo, &mHi
	return out
}

// FormatSalary 把结构化薪资还原为「规整展示文本」。
//
// 全部数值字段为空时返回「薪资未标注」（面议同样落到该分支 —— 原文由 SalaryDesc 保留）。
// 否则按单位输出：月薪 `20-35K·14薪`、日薪 `300-500元/天`、年薪 `30-50万/年`；
// 只有下界（Max 为 nil）时补「以上」，Min 与 Max 相等时只输出一个值。
func FormatSalary(s Salary) string {
	if s.Min == nil && s.Max == nil && s.Months == nil && s.MonthlyMin == nil && s.MonthlyMax == nil {
		return "薪资未标注"
	}
	switch s.Kind {
	case KindNegotiable:
		return "面议"
	case KindUnknown, "":
		return "薪资未标注"
	}

	unit := s.Unit
	if unit == "" {
		unit = UnitMonth
	}

	var body string
	switch unit {
	case UnitDay:
		body = joinRange(s, func(v int) string { return strconv.Itoa(v) }, "元/天")
	case UnitYear:
		body = joinRange(s, func(v int) string { return trimFloat(float64(v) / 10000) }, "万/年")
	default:
		// 月薪统一用 K 展示（20-35K），数值不足 1000 时退回「元」。
		if s.Min != nil && *s.Min < 1000 || s.Max != nil && *s.Max < 1000 {
			body = joinRange(s, func(v int) string { return strconv.Itoa(v) }, "元")
		} else {
			body = joinRange(s, func(v int) string { return trimFloat(float64(v) / 1000) }, "K")
		}
	}
	if body == "" {
		return "薪资未标注"
	}
	if s.Months != nil {
		body += "·" + strconv.Itoa(*s.Months) + "薪"
	}
	return body
}

// MonthlyRange 返回归一到「元/月」的区间，仅当 Kind 为 range 或 single 时有效。
// 其他情况（negotiable / unknown / 自行构造的异常值）返回 nil, nil。
func MonthlyRange(s Salary) (min, max *int) {
	switch s.Kind {
	case KindRange, KindSingle:
		return s.MonthlyMin, s.MonthlyMax
	}
	return nil, nil
}

// joinRange 按 Min/Max 组合数值文本，单位后缀只写一次；
// Max 为 nil 表示只有下界，此时后缀紧跟数值、再接「以上」（如 20K以上）。
func joinRange(s Salary, f func(int) string, suffix string) string {
	switch {
	case s.Min == nil && s.Max == nil:
		return ""
	case s.Min == nil:
		return f(*s.Max) + suffix
	case s.Max == nil:
		return f(*s.Min) + suffix + "以上"
	case *s.Min == *s.Max:
		return f(*s.Min) + suffix
	default:
		return f(*s.Min) + "-" + f(*s.Max) + suffix
	}
}

// trimFloat 去掉小数末尾多余的 0 与小数点。
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// detectUnit 依据原文是否出现「天 / 年」判定单位，缺省按「元/月」。
func detectUnit(core string) string {
	switch {
	case strings.Contains(core, "天"):
		return UnitDay
	case strings.Contains(core, "年"):
		return UnitYear
	default:
		return UnitMonth
	}
}

// findMonths 识别 `14薪`、`13 薪`、`十三薪`、`二十四薪` 等写法，无则返回 nil。
func findMonths(s string) *int {
	if m := reMonthsNum.FindString(s); m != "" {
		digits := strings.TrimSpace(strings.TrimSuffix(m, "薪"))
		if n, err := strconv.Atoi(digits); err == nil && n >= 1 && n <= 36 {
			return &n
		}
	}
	if m := reMonthsCN.FindString(s); m != "" {
		if n := chineseNumber(strings.TrimSpace(strings.TrimSuffix(m, "薪"))); n >= 1 && n <= 36 {
			return &n
		}
	}
	return nil
}

// stripMonths 从文本中移除月数片段及其前置分隔符（· / × / x）。
func stripMonths(s string) string {
	s = reMonthsNum.ReplaceAllString(s, "")
	s = reMonthsCN.ReplaceAllString(s, "")
	s = strings.TrimRight(s, " ·×*xX")
	return strings.TrimSpace(s)
}

// chineseNumber 解析 1~99 的常见中文数字（十三、二十、二十四）。
func chineseNumber(s string) int {
	digits := map[rune]int{
		'一': 1, '二': 2, '三': 3, '四': 4, '五': 5,
		'六': 6, '七': 7, '八': 8, '九': 9,
	}
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	if len(rs) == 1 {
		if rs[0] == '十' {
			return 10
		}
		return digits[rs[0]]
	}
	// 含「十」的两位数：十三 / 二十 / 二十四
	for i, r := range rs {
		if r != '十' {
			continue
		}
		tens := 0
		if i > 0 {
			tens = digits[rs[i-1]]
		}
		if tens == 0 {
			tens = 1 // 「十三」省略了前导一
		}
		ones := 0
		if i+1 < len(rs) {
			ones = digits[rs[i+1]]
		}
		return tens*10 + ones
	}
	return 0
}
