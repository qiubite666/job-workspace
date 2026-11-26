package parser

import (
	"strconv"
	"testing"
	"time"
)

// ip 是表驱动测试里构造 *int 的小工具。
func ip(v int) *int { return &v }

// eqInt 比较两个可空整数是否语义相等（nil 与 nil、同值指针都算相等）。
func eqInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestParseSalary(t *testing.T) {
	cases := []struct {
		name                                     string
		in                                       string
		min, max, months, monthlyMin, monthlyMax *int
		unit, kind                               string
	}{
		{
			name: "区间K加月数", in: "20-35K·14薪",
			min: ip(20000), max: ip(35000), months: ip(14),
			monthlyMin: ip(20000), monthlyMax: ip(35000),
			unit: UnitMonth, kind: KindRange,
		},
		{
			name: "区间K", in: "15-25K",
			min: ip(15000), max: ip(25000),
			monthlyMin: ip(15000), monthlyMax: ip(25000),
			unit: UnitMonth, kind: KindRange,
		},
		{
			name: "双侧K", in: "30K-60K",
			min: ip(30000), max: ip(60000),
			monthlyMin: ip(30000), monthlyMax: ip(60000),
			unit: UnitMonth, kind: KindRange,
		},
		{
			name: "万单位", in: "1-1.5万",
			min: ip(10000), max: ip(15000),
			monthlyMin: ip(10000), monthlyMax: ip(15000),
			unit: UnitMonth, kind: KindRange,
		},
		{
			name: "裸数字按月", in: "8000-12000",
			min: ip(8000), max: ip(12000),
			monthlyMin: ip(8000), monthlyMax: ip(12000),
			unit: UnitMonth, kind: KindRange,
		},
		{
			name: "以上只有下界", in: "20K以上",
			min: ip(20000), max: nil,
			monthlyMin: ip(20000), monthlyMax: ip(20000),
			unit: UnitMonth, kind: KindSingle,
		},
		{
			name: "日薪折算", in: "300-500元/天",
			min: ip(300), max: ip(500),
			monthlyMin: ip(6525), monthlyMax: ip(10875),
			unit: UnitDay, kind: KindRange,
		},
		{
			name: "面议", in: "面议",
			unit: "", kind: KindNegotiable,
		},
		{
			name: "空串", in: "",
			unit: "", kind: KindUnknown,
		},
		{
			name: "薪资保密", in: "薪资保密",
			unit: "", kind: KindUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSalary(tc.in)
			if !eqInt(got.Min, tc.min) {
				t.Errorf("Min = %v, want %v", showInt(got.Min), showInt(tc.min))
			}
			if !eqInt(got.Max, tc.max) {
				t.Errorf("Max = %v, want %v", showInt(got.Max), showInt(tc.max))
			}
			if !eqInt(got.Months, tc.months) {
				t.Errorf("Months = %v, want %v", showInt(got.Months), showInt(tc.months))
			}
			if got.Unit != tc.unit {
				t.Errorf("Unit = %q, want %q", got.Unit, tc.unit)
			}
			if got.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.kind)
			}
			if !eqInt(got.MonthlyMin, tc.monthlyMin) {
				t.Errorf("MonthlyMin = %v, want %v", showInt(got.MonthlyMin), showInt(tc.monthlyMin))
			}
			if !eqInt(got.MonthlyMax, tc.monthlyMax) {
				t.Errorf("MonthlyMax = %v, want %v", showInt(got.MonthlyMax), showInt(tc.monthlyMax))
			}
		})
	}
}

// 容错与边界：全角、脏数据倒序、单值无「以上」、中文月数、负数不出现。
func TestParseSalaryTolerance(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"全角字符", "２０－３５Ｋ·１４薪"},
		{"倒序区间", "35-20K"},
		{"中文月数", "20-35K·十三薪"},
		{"单值无以上", "25K"},
		{"乱码", "薪资美丽"},
		{"超长空格", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSalary(tc.in) // 只要求不 panic、不负数
			for _, p := range []*int{got.Min, got.Max, got.Months, got.MonthlyMin, got.MonthlyMax} {
				if p != nil && *p < 0 {
					t.Fatalf("出现负数: %d", *p)
				}
			}
		})
	}

	if got := ParseSalary("35-20K"); !eqInt(got.Min, ip(20000)) || !eqInt(got.Max, ip(35000)) {
		t.Errorf("倒序区间未交换: %v-%v", showInt(got.Min), showInt(got.Max))
	}
	if got := ParseSalary("25K"); got.Kind != KindSingle || !eqInt(got.Min, ip(25000)) || !eqInt(got.Max, ip(25000)) {
		t.Errorf("25K 期望 single 且 Min=Max=25000, 实际 %+v", got)
	}
	if got := ParseSalary("20-35K·十三薪"); !eqInt(got.Months, ip(13)) {
		t.Errorf("十三薪 Months = %v, want 13", showInt(got.Months))
	}
}

// 额外形态：年薪、带月数的万区间、双侧带 K（避免「K」被吞掉）。
func TestParseSalaryExtraFormats(t *testing.T) {
	cases := []struct {
		in                                       string
		min, max, months, monthlyMin, monthlyMax *int
		unit, kind                               string
	}{
		{"30-50万/年", ip(300000), ip(500000), nil, ip(25000), ip(41667), UnitYear, KindRange},
		{"1.5-2万·13薪", ip(15000), ip(20000), ip(13), ip(15000), ip(20000), UnitMonth, KindRange},
		{"20K-35K", ip(20000), ip(35000), nil, ip(20000), ip(35000), UnitMonth, KindRange},
	}
	for _, tc := range cases {
		got := ParseSalary(tc.in)
		if !eqInt(got.Min, tc.min) || !eqInt(got.Max, tc.max) ||
			!eqInt(got.Months, tc.months) || !eqInt(got.MonthlyMin, tc.monthlyMin) ||
			!eqInt(got.MonthlyMax, tc.monthlyMax) || got.Unit != tc.unit || got.Kind != tc.kind {
			t.Errorf("%q 解析结果不符: %+v (min=%v max=%v months=%v mmin=%v mmax=%v %s/%s)",
				tc.in, got, showInt(got.Min), showInt(got.Max), showInt(got.Months),
				showInt(got.MonthlyMin), showInt(got.MonthlyMax), got.Unit, got.Kind)
		}
	}
}

func TestFormatSalary(t *testing.T) {
	cases := []struct{ in, want string }{
		{"20-35K·14薪", "20-35K·14薪"},
		{"300-500元/天", "300-500元/天"},
		{"20K以上", "20K以上"},
		{"面议", "薪资未标注"},
		{"", "薪资未标注"},
		{"薪资保密", "薪资未标注"},
	}
	for _, tc := range cases {
		if got := FormatSalary(ParseSalary(tc.in)); got != tc.want {
			t.Errorf("FormatSalary(ParseSalary(%q)) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMonthlyRange(t *testing.T) {
	min, max := MonthlyRange(ParseSalary("20-35K"))
	if !eqInt(min, ip(20000)) || !eqInt(max, ip(35000)) {
		t.Errorf("range 应返回月薪区间, got %v-%v", showInt(min), showInt(max))
	}
	min, max = MonthlyRange(ParseSalary("面议"))
	if min != nil || max != nil {
		t.Errorf("negotiable 应返回 nil,nil, got %v-%v", showInt(min), showInt(max))
	}
	min, max = MonthlyRange(ParseSalary(""))
	if min != nil || max != nil {
		t.Errorf("unknown 应返回 nil,nil, got %v-%v", showInt(min), showInt(max))
	}
}

func TestParsePublishTime(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	sec := strconv.FormatInt(now.Unix(), 10)

	cases := []struct {
		name    string
		in      []string
		wantISO string
		wantSrc string
	}{
		{"13位毫秒时间戳", []string{ms}, "2026-11-26T10:00:00Z", "api"},
		{"10位秒时间戳", []string{sec}, "2026-11-26T10:00:00Z", "api"},
		{"日期", []string{"2026-11-24"}, "2026-11-24T00:00:00Z", "api"},
		{"日期时间", []string{"2026-11-24 10:30"}, "2026-11-24T10:30:00Z", "api"},
		{"斜杠日期", []string{"2026/11/24"}, "2026-11-24T00:00:00Z", "api"},
		{"带时区ISO8601", []string{"2026-11-24T10:30:00+08:00"}, "2026-11-24T02:30:00Z", "api"},
		{"昨天", []string{"昨天"}, "2026-11-25T10:00:00Z", "api"},
		{"刚刚", []string{"刚刚"}, "2026-11-26T10:00:00Z", "api"},
		{"N天前", []string{"3天前"}, "2026-11-23T10:00:00Z", "api"},
		{"N小时前", []string{"2小时前"}, "2026-11-26T08:00:00Z", "api"},
		{"N分钟前", []string{"30分钟前"}, "2026-11-26T09:30:00Z", "api"},
		{"无年份月日", []string{"11-24"}, "2026-11-24T00:00:00Z", "api"},
		{"跳过空串取下一个", []string{"", "  ", "2026-11-24"}, "2026-11-24T00:00:00Z", "api"},
		{"跳过无效取下一个", []string{"乱码", "2026-11-24"}, "2026-11-24T00:00:00Z", "api"},
		{"空候选数组", nil, "", "missing"},
		{"全部无效", []string{"", "  ", "不知道"}, "", "missing"},
		{"超出三年下界", []string{"2020-01-01"}, "", "missing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iso, src := ParsePublishTime(tc.in, now)
			if iso != tc.wantISO || src != tc.wantSrc {
				t.Errorf("ParsePublishTime(%v) = (%q, %q), want (%q, %q)",
					tc.in, iso, src, tc.wantISO, tc.wantSrc)
			}
		})
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	cases := []struct{ in, want string }{
		{"2026-11-26T08:00:00Z", "今天"},
		{"2026-11-25T08:00:00Z", "昨天"},
		{"2026-11-23T08:00:00Z", "3天前"},
		{"2026-10-27T08:00:00Z", "30天前"},
		{"2026-10-20T08:00:00Z", "2026-10-20"},
		{"2026-01-05T00:00:00Z", "2026-01-05"},
		{"", "未提供"},
		{"not-a-time", "未提供"},
	}
	for _, tc := range cases {
		if got := RelativeTime(tc.in, now); got != tc.want {
			t.Errorf("RelativeTime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 跨月、跨年、闰年都必须正确，且空值返回 nil。
func TestDayDiff(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want *int
	}{
		{"跨年", "2025-12-31", "2026-01-01", ip(1)},
		{"跨年反向", "2026-01-01", "2025-12-31", ip(-1)},
		{"闰年二月", "2024-02-28", "2024-03-01", ip(2)},
		{"普通跨月", "2026-11-24", "2026-11-26", ip(2)},
		{"同一天", "2026-11-26", "2026-11-26", ip(0)},
		{"a为空", "", "2026-01-01", nil},
		{"b为空", "2026-01-01", "", nil},
		{"非法", "bad", "2026-01-01", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DayDiff(tc.a, tc.b)
			if !eqInt(got, tc.want) {
				t.Errorf("DayDiff(%q, %q) = %v, want %v", tc.a, tc.b, showInt(got), showInt(tc.want))
			}
		})
	}
}

// showInt 便于在失败信息里区分 nil 与具体值。
func showInt(v *int) string {
	if v == nil {
		return "nil"
	}
	return strconv.Itoa(*v)
}
