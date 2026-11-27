package userdata

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClearAllRemovesOnlyJSON 只删账号状态文件，不碰目录内其它文件。
func TestClearAllRemovesOnlyJSON(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	if _, err := st.Save("alice", State{Favorites: []string{"boss:1"}}); err != nil {
		t.Fatalf("写入 alice 失败: %v", err)
	}
	if _, err := st.Save("bob", State{Notes: map[string]string{}}); err != nil {
		t.Fatalf("写入 bob 失败: %v", err)
	}
	// 混入一个非 json 文件，清空时不应被删。
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatalf("写入 keep.txt 失败: %v", err)
	}

	removed, err := st.ClearAll()
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if removed != 2 {
		t.Errorf("删除文件数 = %d，期望 2", removed)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("非 json 文件被误删")
	}

	// 清空后读取应为空状态，而不是报错。
	if got, err := st.Load("alice"); err != nil {
		t.Errorf("清空后读取不应报错: %v", err)
	} else if len(got.Favorites) != 0 {
		t.Errorf("清空后仍有数据: %v", got.Favorites)
	}
}

// TestClearAllOnMissingDir 目录不存在时应返回 0 而不是错误。
func TestClearAllOnMissingDir(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "not-exist"))
	removed, err := st.ClearAll()
	if err != nil {
		t.Fatalf("目录不存在时不应报错: %v", err)
	}
	if removed != 0 {
		t.Errorf("返回 %d，期望 0", removed)
	}
}
