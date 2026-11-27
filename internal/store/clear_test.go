package store

import (
	"os"
	"path/filepath"
	"testing"

	"jobworkspace/internal/model"
)

// TestClearJobs 清空岗位后文件应为空数组，且采集报告被一并删除。
func TestClearJobs(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)

	jobs := []model.Job{
		newTestJob("boss:1", "1"),
		newTestJob("boss:2", "2"),
		newTestJob("boss:3", "3"),
	}
	if _, _, err := st.MergeAndSave(jobs); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	if err := st.SaveReport(model.Report{OK: true}); err != nil {
		t.Fatalf("写入报告失败: %v", err)
	}

	removed, err := st.ClearJobs()
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if removed != 3 {
		t.Errorf("返回条数 = %d，期望 3", removed)
	}

	got, _, err := st.LoadJobs()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("清空后仍有 %d 条岗位", len(got))
	}

	// 文件必须还在（内容是 []），否则下游会以为是"从未采集过"。
	raw, err := os.ReadFile(filepath.Join(dir, JobsFile))
	if err != nil {
		t.Fatalf("jobs 文件不应被删除: %v", err)
	}
	if len(raw) == 0 {
		t.Error("jobs 文件内容为空，期望 []")
	}

	if _, err := os.Stat(filepath.Join(dir, ReportFile)); !os.IsNotExist(err) {
		t.Error("采集报告应被删除")
	}
}

// TestClearJobsOnEmpty 空库上清空应成功且返回 0，不报错。
func TestClearJobsOnEmpty(t *testing.T) {
	st := New(t.TempDir())
	removed, err := st.ClearJobs()
	if err != nil {
		t.Fatalf("空库清空不应报错: %v", err)
	}
	if removed != 0 {
		t.Errorf("空库返回 %d，期望 0", removed)
	}
}
