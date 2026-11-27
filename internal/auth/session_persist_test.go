package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSessionsSurviveRestart 会话落盘后重载仍可用，这是"重启不掉线"的核心保证。
func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	m1 := NewSessionManager(time.Hour)
	sess, err := m1.Create(PublicUser{Username: "alice", Role: RoleAdmin})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := m1.SaveSessions(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 模拟进程重启
	m2 := NewSessionManager(time.Hour)
	if err := m2.LoadSessions(path); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	got, ok := m2.Get(sess.ID)
	if !ok {
		t.Fatal("重启后会话失效")
	}
	if got.Username != "alice" || got.Role != RoleAdmin {
		t.Errorf("恢复的会话内容不对: %+v", got)
	}
}

// TestExpiredSessionsNotRestored 过期会话不恢复，避免"重启复活"已登出的旧会话。
func TestExpiredSessionsNotRestored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	m1 := NewSessionManager(time.Hour)
	sess, _ := m1.Create(PublicUser{Username: "bob", Role: RoleViewer})
	// 手动把过期时间改到过去
	m1.mu.Lock()
	s := m1.sessions[sess.ID]
	s.ExpiresAt = time.Now().Add(-time.Minute)
	m1.sessions[sess.ID] = s
	m1.mu.Unlock()

	if err := m1.SaveSessions(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	m2 := NewSessionManager(time.Hour)
	if err := m2.LoadSessions(path); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, ok := m2.Get(sess.ID); ok {
		t.Error("过期会话被恢复了")
	}
}

// TestSessionFileIsPrivate 会话 ID 等同凭据，文件必须 0600。
func TestSessionFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	m := NewSessionManager(time.Hour)
	if _, err := m.Create(PublicUser{Username: "carol", Role: RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveSessions(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上 chmod 只能切换只读位，权限位没有意义；生产是 Linux，在那里才校验。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 POSIX 权限位")
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("文件权限 = %o，期望 600", perm)
	}
}

// TestLoadMissingFileIsOK 首次启动没有会话文件，不应报错。
func TestLoadMissingFileIsOK(t *testing.T) {
	m := NewSessionManager(time.Hour)
	if err := m.LoadSessions(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("文件不存在时返回错误: %v", err)
	}
}
