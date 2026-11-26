package store

import (
	"strings"
	"testing"

	"jobworkspace/internal/model"
)

// newTestJob 造一条最小可用的岗位，字段按需覆盖。
func newTestJob(id, externalID string) model.Job {
	return model.Job{
		ID:           id,
		Platform:     "boss",
		PlatformName: "BOSS直聘",
		ExternalID:   externalID,
		JobName:      "运维工程师",
		BrandName:    "青城数据",
		CityName:     "呼和浩特",
	}
}

// TestMergePreservingKeepsDescription 列表上报不得抹掉详情补全的 JD。
//
// 这是最容易回归的一处：列表接口不返回职位描述，如果合并时整条覆盖，
// 用户点开详情补好的 JD 会在下一次翻页时瞬间消失。
func TestMergePreservingKeepsDescription(t *testing.T) {
	st := New(t.TempDir())

	list := newTestJob("boss:1", "1")
	if _, _, err := st.MergeAndSave([]model.Job{list}); err != nil {
		t.Fatalf("首次落库失败: %v", err)
	}

	detail := newTestJob("boss:1", "1")
	detail.Description = "岗位职责：\n1. 负责 K8s 运维"
	detail.ActiveTimeDesc = "刚刚活跃"
	updated, added, _, err := st.UpsertDetails([]model.Job{detail})
	if err != nil {
		t.Fatalf("详情落库失败: %v", err)
	}
	if updated != 1 || added != 0 {
		t.Fatalf("详情应命中已有记录: updated=%d added=%d", updated, added)
	}

	// 再次上报列表（不带 JD）
	if _, _, err := st.MergeAndSave([]model.Job{newTestJob("boss:1", "1")}); err != nil {
		t.Fatalf("列表二次落库失败: %v", err)
	}

	jobs, _, err := st.LoadJobs()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("期望 1 条，实际 %d 条", len(jobs))
	}
	if !strings.Contains(jobs[0].Description, "K8s") {
		t.Errorf("JD 被列表上报抹掉了: %q", jobs[0].Description)
	}
	if jobs[0].ActiveTimeDesc != "刚刚活跃" {
		t.Errorf("活跃状态被抹掉: %q", jobs[0].ActiveTimeDesc)
	}
}

// TestUpsertDetailsFillsFields 详情包应补全已有记录，而不是清空列表带来的字段。
func TestUpsertDetailsFillsFields(t *testing.T) {
	st := New(t.TempDir())

	list := newTestJob("boss:2", "2")
	list.SalaryDesc = "15-25K"
	list.SalaryText = "15-25K"
	list.Skills = []string{"Linux"}
	if _, _, err := st.MergeAndSave([]model.Job{list}); err != nil {
		t.Fatalf("首次落库失败: %v", err)
	}

	// 详情包字段少：只有描述与活跃度
	detail := model.Job{ID: "boss:2", Platform: "boss", ExternalID: "2",
		Description: "JD 正文", ActiveTimeDesc: "今日活跃"}
	if _, _, _, err := st.UpsertDetails([]model.Job{detail}); err != nil {
		t.Fatalf("详情落库失败: %v", err)
	}

	jobs, _, _ := st.LoadJobs()
	got := jobs[0]
	if got.Description != "JD 正文" {
		t.Errorf("描述未写入: %q", got.Description)
	}
	if got.SalaryDesc != "15-25K" || len(got.Skills) != 1 {
		t.Errorf("列表字段被详情包清空: salary=%q skills=%v", got.SalaryDesc, got.Skills)
	}
	if got.JobName != "运维工程师" {
		t.Errorf("岗位名被清空: %q", got.JobName)
	}
}

// TestUpsertDetailsInsertsUnknown 详情包遇到库里没有的岗位时按新增处理。
func TestUpsertDetailsInsertsUnknown(t *testing.T) {
	st := New(t.TempDir())
	detail := model.Job{ID: "boss:9", Platform: "boss", ExternalID: "9",
		JobName: "新岗位", BrandName: "某公司", Description: "JD"}

	updated, added, total, err := st.UpsertDetails([]model.Job{detail})
	if err != nil {
		t.Fatalf("详情落库失败: %v", err)
	}
	if updated != 0 || added != 1 || total != 1 {
		t.Fatalf("期望 updated=0 added=1 total=1，实际 %d/%d/%d", updated, added, total)
	}
}
