package library

import (
	"testing"

	"jobworkspace/internal/model"
)

func job(id string) model.Job {
	return model.Job{ID: id, Platform: "boss", ExternalID: id, JobName: "岗位" + id}
}

// TestIsolation 两个账号的职位库必须完全隔离。
func TestIsolation(t *testing.T) {
	st := New(t.TempDir())

	if _, _, err := st.Merge("alice", []model.Job{job("a1"), job("a2")}); err != nil {
		t.Fatalf("alice 写入失败: %v", err)
	}
	if _, _, err := st.Merge("bob", []model.Job{job("b1")}); err != nil {
		t.Fatalf("bob 写入失败: %v", err)
	}

	a, _, _ := st.Load("alice")
	b, _, _ := st.Load("bob")
	if len(a) != 2 || len(b) != 1 {
		t.Fatalf("库大小不对: alice=%d bob=%d", len(a), len(b))
	}
	for _, j := range b {
		if j.ID == "a1" || j.ID == "a2" {
			t.Fatalf("bob 的库里出现了 alice 的数据: %s", j.ID)
		}
	}
}

// TestClearOnlyOwn 清空一个账号不得影响另一个账号。
func TestClearOnlyOwn(t *testing.T) {
	st := New(t.TempDir())
	if _, _, err := st.Merge("alice", []model.Job{job("a1")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Merge("bob", []model.Job{job("b1")}); err != nil {
		t.Fatal(err)
	}

	removed, err := st.Clear("alice")
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if removed != 1 {
		t.Errorf("清空条数 = %d，期望 1", removed)
	}

	a, _, _ := st.Load("alice")
	if len(a) != 0 {
		t.Errorf("alice 未清空: %d 条", len(a))
	}
	b, _, _ := st.Load("bob")
	if len(b) != 1 {
		t.Errorf("bob 被误清空: %d 条", len(b))
	}
}

// TestBadUsernameRejected 账号名必须挡住路径穿越。
func TestBadUsernameRejected(t *testing.T) {
	st := New(t.TempDir())
	for _, name := range []string{"../evil", "a/b", "", "ab", "A B", ".."} {
		if _, _, err := st.Merge(name, []model.Job{job("x")}); err == nil {
			t.Errorf("非法账号名 %q 未被拒绝", name)
		}
	}
}

// TestDetailMergeIsolated 详情包只能落到令牌所属账号。
func TestDetailMergeIsolated(t *testing.T) {
	st := New(t.TempDir())
	if _, _, err := st.Merge("alice", []model.Job{job("x1")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Merge("bob", []model.Job{job("x1")}); err != nil {
		t.Fatal(err)
	}

	detail := job("x1")
	detail.Description = "alice 的 JD"
	updated, _, _, err := st.UpsertDetails("alice", []model.Job{detail})
	if err != nil {
		t.Fatalf("详情写入失败: %v", err)
	}
	if updated != 1 {
		t.Errorf("expect updated=1, got %d", updated)
	}

	a, _, _ := st.Load("alice")
	b, _, _ := st.Load("bob")
	if a[0].Description != "alice 的 JD" {
		t.Errorf("alice 的 JD 未写入: %q", a[0].Description)
	}
	if b[0].Description != "" {
		t.Errorf("bob 的库被串写了: %q", b[0].Description)
	}
}

// TestAdoptLegacy 旧全局库只迁移一次，且不覆盖已有数据。
func TestAdoptLegacy(t *testing.T) {
	dir := t.TempDir()
	st := New(dir + "/libraries")

	legacy := dir + "/jobs.json"
	// 直接构造旧文件
	if err := writeJobs(legacy, []model.Job{job("old1"), job("old2")}); err != nil {
		t.Fatal(err)
	}

	n, err := st.AdoptLegacy(legacy, "alice")
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if n != 2 {
		t.Errorf("迁移条数 = %d，期望 2", n)
	}
	got, _, _ := st.Load("alice")
	if len(got) != 2 {
		t.Errorf("迁移后 alice 库大小 = %d", len(got))
	}

	// 第二次不应重复迁移
	if n2, _ := st.AdoptLegacy(legacy, "alice"); n2 != 0 {
		t.Errorf("重复迁移了 %d 条", n2)
	}
}
