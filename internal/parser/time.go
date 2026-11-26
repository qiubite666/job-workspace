package parser

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 时间候选值中可能出现的相对时间与无年份日期写法。
var (
	reMinuteAgo = regexp.MustCompile(`^(\d+)\s*分钟前$`)
	reHourAgo   = regexp.MustCompile(`^(\d+)\s*(小时|个小时)前$`)
	reDayAgo    = regexp.MustCompile(`^(\d+)\s*天前$`)
	reMonthDay  = regexp.MustCompile(`^(\d{1,2})\s*[-/]\s*(\d{1,2})$`)
	reCnDate    = regexp.MustCompile(`^(\d{4})\s*年\s*(\d{1,2})\s*月\s*(\d{1,2})\s*日?$`)
	reCnMonthDy = regexp.MustCompile(`^(\d{1,2})\s*月\s*(\d{1,2})\s*日$`)
)

// 时间文本里的全角字符规整（日期里不会出现「至/到」，故单独一套替换器）。
var timeNormalizer = strings.NewReplacer(
	"０", "0", "１", "1", "２", "2", "３", "3", "４", "4",
	"５", "5", "６", "6", "７", "7", "８", "8", "９", "9",
	"／", "/", "：", ":", "．", ".", "－", "-",
	"　", " ",
)

// 绝对时间布局：从最严格到最宽松，无时区的布局按 now 的时区解释。
var absoluteLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02 15:04",
	"2006/01/02",
	"2006.01.02 15:04:05",
	"2006.01.02",
	"20060102",
}

// flexLayouts 用于 RelativeTime / DayDiff 的宽松解析（只用到日期部分）。
var flexLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02 15:04",
	"2006/01/02",
	"2006.01.02",
}

// ParsePublishTime 按优先级依次尝试发布时间候选值。
//
// 每个候选依次尝试：13 位毫秒/10 位秒时间戳、RFC3339/ISO8601、
// `2026-11-24`、`2026/11/24`、`2026-11-24 10:30`、无年份的 `11-24`、
// `今天/昨天/前天/刚刚`、`N分钟前/N小时前/N天前`。
//
// 有效区间为 [now-3年, now+1天]（防止脏数据把「1970」「2099」写进库），
// 越界的候选视为无效并继续尝试下一个。全部候选失败返回 ("", "missing")；
// 任一个成功返回 (RFC3339 UTC, "api")。空数组与空字符串元素会被安全跳过。
func ParsePublishTime(candidates []string, now time.Time) (iso string, source string) {
	lower := now.AddDate(-3, 0, 0)
	upper := now.AddDate(0, 0, 1)
	for _, c := range candidates {
		raw := strings.TrimSpace(c)
		if raw == "" {
			continue
		}
		t, ok := parseTimeCandidate(raw, now)
		if !ok {
			continue
		}
		if t.Before(lower) || t.After(upper) {
			continue
		}
		return t.UTC().Format(time.RFC3339), "api"
	}
	return "", "missing"
}

// parseTimeCandidate 解析单个候选值，失败返回 false（绝不 panic）。
func parseTimeCandidate(raw string, now time.Time) (time.Time, bool) {
	s := strings.TrimSpace(timeNormalizer.Replace(raw))
	if s == "" {
		return time.Time{}, false
	}

	// 纯数字：13 位毫秒 / 10 位秒时间戳。
	// 8 位（yyyyMMdd）交给后面的布局处理，避免被误当秒级时间戳。
	if isAllDigits(s) {
		switch len(s) {
		case 13:
			if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
				return time.Unix(0, ms*int64(time.Millisecond)), true
			}
		case 10:
			if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
				return time.Unix(sec, 0), true
			}
		}
	}

	if t, ok := parseRelativeTime(s, now); ok {
		return t, true
	}

	loc := now.Location()
	for _, layout := range absoluteLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}

	// 中文日期：2026年11月24日 / 11月24日
	if m := reCnDate.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return makeDate(y, mo, d, loc)
	}
	if m := reCnMonthDy.FindStringSubmatch(s); m != nil {
		mo, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		return inferYearDate(mo, d, now)
	}

	// 无年份日期：11-24 / 11/24（补齐 now 所在年，超前 30 天以上判去年）
	if m := reMonthDay.FindStringSubmatch(s); m != nil {
		mo, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		return inferYearDate(mo, d, now)
	}
	return time.Time{}, false
}

// makeDate 构造当日零点；非法月日（如 2-30）返回 false。
func makeDate(y, mo, d int, loc *time.Location) (time.Time, bool) {
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
	if int(t.Month()) != mo || t.Day() != d { // time.Date 会把 2-30 归一到 3-2
		return time.Time{}, false
	}
	return t, true
}

// inferYearDate 给无年份的月日补年份：默认取 now 所在年，
// 若该日期比 now 晚 30 天以上，则判定为去年。
func inferYearDate(mo, d int, now time.Time) (time.Time, bool) {
	t, ok := makeDate(now.Year(), mo, d, now.Location())
	if !ok {
		return time.Time{}, false
	}
	if t.After(now.AddDate(0, 0, 30)) {
		return makeDate(now.Year()-1, mo, d, now.Location())
	}
	return t, true
}

// parseRelativeTime 处理「今天/昨天/前天/刚刚」与「N分钟前/N小时前/N天前」。
func parseRelativeTime(s string, now time.Time) (time.Time, bool) {
	switch s {
	case "刚刚", "刚才":
		return now, true
	case "今天", "今日":
		return now, true
	case "昨天", "昨日":
		return now.AddDate(0, 0, -1), true
	case "前天":
		return now.AddDate(0, 0, -2), true
	}
	if m := reMinuteAgo.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return now.Add(-time.Duration(n) * time.Minute), true
		}
	}
	if m := reHourAgo.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return now.Add(-time.Duration(n) * time.Hour), true
		}
	}
	if m := reDayAgo.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return now.AddDate(0, 0, -n), true
		}
	}
	return time.Time{}, false
}

// RelativeTime 把 ISO 时间渲染为人读字符串。
//
// 规则：与 now 同日 → 「今天」；差 1 天 → 「昨天」；差 2~30 天 → 「N天前」；
// 超过 30 天 → 「YYYY-MM-DD」；iso 为空或无法解析 → 「未提供」。
// 日期比较按日历日计算，跨月跨年都使用 time 包，不手写月份天数。
func RelativeTime(iso string, now time.Time) string {
	t, ok := parseFlexible(iso)
	if !ok {
		return "未提供"
	}
	loc := now.Location()
	days := civilDays(now) - civilDays(t.In(loc))
	switch {
	case days <= 0:
		return "今天"
	case days == 1:
		return "昨天"
	case days <= 30:
		return strconv.Itoa(int(days)) + "天前"
	default:
		return t.In(loc).Format("2006-01-02")
	}
}

// DayDiff 返回 bISO - aISO 的整数天数，用于计算「距上次跟进过了几天」。
//
// 任一参数为空串或非法时间时返回 nil；计算基于日历日，跨月跨年正确
// （例如 2025-12-31 → 2026-01-01 返回 1，闰年 2 月也正确处理）。
func DayDiff(aISO, bISO string) *int {
	a, okA := parseFlexible(aISO)
	b, okB := parseFlexible(bISO)
	if !okA || !okB {
		return nil
	}
	diff := int(civilDays(b) - civilDays(a))
	return &diff
}

// parseFlexible 宽松解析时间字符串；空串直接失败。
func parseFlexible(raw string) (time.Time, bool) {
	s := strings.TrimSpace(timeNormalizer.Replace(raw))
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range flexLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// civilDays 返回该时刻所在「公历日」的 UTC 天数序号，用于纯日期差值计算。
func civilDays(t time.Time) int64 {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// isAllDigits 判断是否全为 ASCII 数字且非空。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
