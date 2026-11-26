package dedupe

import (
	"testing"

	"jobworkspace/internal/model"
)

// ptr 构造 *int，用于薪资指针字段。
func ptr(v int) *int { return &v }

// fullJob 构造一条字段齐全、完整度较高的岗位，便于各用例在此基础上做减法。
func fullJob(id string) model.Job {
	return model.Job{
		ID:               id,
		Platform:         "boss",
		PlatformName:     "BOSS直聘",
		ExternalID:       id,
		JobName:          "Java开发工程师",
		BrandName:        "字节跳动",
		CityName:         "北京",
		AreaDistrict:     "海淀区",
		BusinessDistrict: "中关村",
		SalaryDesc:       "20-35K·14薪",
		SalaryKind:       "range",
		SalaryMin:        ptr(20000),
		SalaryMax:        ptr(35000),
		SalaryMonthlyMin: ptr(20000),
		SalaryMonthlyMax: ptr(35000),
		Experience:       "3-5年",
		Degree:           "本科",
		JobLabels:        []string{"3-5年", "本科"},
		Skills:           []string{"Java", "Spring Boot"},
		WelfareList:      []string{"五险一金"},
		BossName:         "李女士",
		PublishedAt:      "2026-11-20T00:00:00Z",
		JobURL:           "https://www.zhipin.com/job_detail/" + id,
		CollectedAt:      "2026-11-24T10:00:00Z",
	}
}

// TestSameIDKeepsMoreComplete 同 ID 不同薪资/完整度 → 只保留更完整的一条。
func TestSameIDKeepsMoreComplete(t *testing.T) {
	full := fullJob("boss:1")

	poor := model.Job{
		ID:          "boss:1",
		Platform:    "boss",
		JobName:     "Java开发工程师",
		BrandName:   "字节跳动",
		CityName:    "北京",
		SalaryDesc:  "面议",
		SalaryKind:  "negotiable",
		CollectedAt: "2026-11-25T10:00:00Z", // 更新但信息更少，不应胜出
	}

	out, removed, byID, byFP := Jobs([]model.Job{poor, full})
	if len(out) != 1 {
		t.Fatalf("期望合并为 1 条，实际 %d 条", len(out))
	}
	if removed != 1 || byID != 1 || byFP != 0 {
		t.Fatalf("计数错误：removed=%d byID=%d byFingerprint=%d", removed, byID, byFP)
	}
	if out[0].SalaryKind != "range" || len(out[0].Skills) == 0 {
		t.Fatalf("应保留更完整的一条，实际 SalaryKind=%q skills=%v", out[0].SalaryKind, out[0].Skills)
	}
}

// TestSameFingerprintMerged 同平台同指纹不同 ID（重复曝光）→ 合并为 1 条。
func TestSameFingerprintMerged(t *testing.T) {
	a := fullJob("boss:1")
	b := fullJob("boss:2")

	out, removed, byID, byFP := Jobs([]model.Job{a, b})
	if len(out) != 1 {
		t.Fatalf("期望指纹合并为 1 条，实际 %d 条", len(out))
	}
	if removed != 1 || byID != 0 || byFP != 1 {
		t.Fatalf("计数错误：removed=%d byID=%d byFingerprint=%d", removed, byID, byFP)
	}
}

// TestDifferentCompanySameJobName 不同公司同名岗位 → 保留 2 条。
func TestDifferentCompanySameJobName(t *testing.T) {
	a := fullJob("boss:1")
	b := fullJob("boss:2")
	b.BrandName = "美团"
	b.JobURL = "https://www.zhipin.com/job_detail/boss:2"

	out, removed, byID, byFP := Jobs([]model.Job{a, b})
	if len(out) != 2 {
		t.Fatalf("不同公司同名岗位应保留 2 条，实际 %d 条", len(out))
	}
	if removed != 0 || byID != 0 || byFP != 0 {
		t.Fatalf("不应有移除：removed=%d byID=%d byFingerprint=%d", removed, byID, byFP)
	}
}

// TestDifferentPlatformSameFingerprint 指纹只在同平台内生效 → 跨平台保留 2 条。
func TestDifferentPlatformSameFingerprint(t *testing.T) {
	a := fullJob("boss:1")
	b := fullJob("liepin:1")
	b.Platform = "liepin"
	b.PlatformName = "猎聘"

	out, _, _, byFP := Jobs([]model.Job{a, b})
	if len(out) != 2 || byFP != 0 {
		t.Fatalf("跨平台不应按指纹合并：len=%d byFingerprint=%d", len(out), byFP)
	}
}

// TestNewerCollectedAtWins 完整度与薪资明确度都相同时，保留采集时间更新的一条。
func TestNewerCollectedAtWins(t *testing.T) {
	older := fullJob("boss:2")
	fresh := fullJob("boss:2")
	fresh.CollectedAt = "2026-11-26T08:00:00Z"

	out, _, byID, _ := Jobs([]model.Job{older, fresh})
	if len(out) != 1 || byID != 1 {
		t.Fatalf("期望按 ID 合并：len=%d byID=%d", len(out), byID)
	}
	if out[0].CollectedAt != fresh.CollectedAt {
		t.Fatalf("应保留更新的 CollectedAt，实际 %q", out[0].CollectedAt)
	}
}

// TestEmptyInput 空输入与 nil 输入都返回空切片而非 nil，且不 panic。
func TestEmptyInput(t *testing.T) {
	for _, in := range [][]model.Job{nil, {}} {
		out, removed, byID, byFP := Jobs(in)
		if out == nil {
			t.Fatal("空输入应返回非 nil 空切片")
		}
		if len(out) != 0 || removed != 0 || byID != 0 || byFP != 0 {
			t.Fatalf("空输入计数应为 0：len=%d removed=%d byID=%d byFP=%d", len(out), removed, byID, byFP)
		}
	}
}

// TestOrderStability 输出保持首次出现顺序；替换内容不改变位置；不修改入参。
func TestOrderStability(t *testing.T) {
	a := model.Job{ // 首条，信息较少
		ID:           "boss:a",
		Platform:     "boss",
		JobName:      "Java开发工程师",
		BrandName:    "字节跳动",
		CityName:     "北京",
		AreaDistrict: "海淀区",
		CollectedAt:  "2026-11-24T10:00:00Z",
	}
	b := fullJob("boss:b")
	b.BrandName = "美团"

	aBetter := fullJob("boss:a2") // 与 a 同指纹，信息更全
	aBetter.BrandName = "字节跳动"

	in := []model.Job{a, b, aBetter}
	out, _, _, byFP := Jobs(in)

	if len(out) != 2 {
		t.Fatalf("期望 2 条，实际 %d 条", len(out))
	}
	if byFP != 1 {
		t.Fatalf("期望按指纹合并 1 条，实际 %d", byFP)
	}
	// 首位置归 a 那一组，内容被 aBetter 替换。
	if out[0].ID != "boss:a2" || out[1].ID != "boss:b" {
		t.Fatalf("顺序不稳定：%q, %q", out[0].ID, out[1].ID)
	}
	// 入参 slice 不得被改写。
	if in[0].ID != "boss:a" || len(in) != 3 {
		t.Fatalf("入参被修改：len=%d first=%q", len(in), in[0].ID)
	}
}

// TestCollectedAtAcrossTimezones 带不同时区偏移的 CollectedAt 按真实时间比新旧。
//
// 东八区 10:00（=02:00Z）看起来字符串更大，但实际早于 05:00Z，应保留后者。
func TestCollectedAtAcrossTimezones(t *testing.T) {
	east8 := fullJob("boss:1")
	east8.CollectedAt = "2026-11-26T10:00:00+08:00"

	utc := fullJob("boss:2")
	utc.CollectedAt = "2026-11-26T05:00:00Z"

	out, _, _, byFP := Jobs([]model.Job{east8, utc})
	if len(out) != 1 || byFP != 1 {
		t.Fatalf("期望按指纹合并为 1 条：len=%d byFingerprint=%d", len(out), byFP)
	}
	if out[0].CollectedAt != utc.CollectedAt {
		t.Fatalf("应保留真实时间更新的一条（05:00Z），实际 %q", out[0].CollectedAt)
	}
}

// TestDegenerateFingerprintNotMerged 缺少公司/岗位/城市/区域的记录不按指纹合并。
func TestDegenerateFingerprintNotMerged(t *testing.T) {
	a := model.Job{Platform: "boss", CollectedAt: "2026-11-24T10:00:00Z"}
	b := model.Job{Platform: "boss", CollectedAt: "2026-11-24T11:00:00Z"}

	out, _, _, byFP := Jobs([]model.Job{a, b})
	if len(out) != 2 || byFP != 0 {
		t.Fatalf("退化指纹不应合并：len=%d byFingerprint=%d", len(out), byFP)
	}
}
