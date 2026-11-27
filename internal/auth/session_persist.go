package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// sessionRecord 是会话落盘形态。
//
// 只存必要字段：ID 本身就是凭据，所以文件必须是 0600，
// 且只允许服务进程读取。落盘目的是"重启不掉线"，不是共享会话。
type sessionRecord struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// LoadSessions 从文件恢复未过期会话。
//
// 文件不存在视为"首次启动"，返回 nil；解析失败也只返回错误，
// 由调用方决定是否继续（不能因为会话文件坏了就拒绝启动）。
func (m *SessionManager) LoadSessions(path string) error {
	if path == "" {
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var recs []sessionRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		return err
	}

	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	restored := 0
	for _, r := range recs {
		if r.ID == "" || r.Username == "" {
			continue
		}
		if !now.Before(r.ExpiresAt) {
			continue // 过期的不恢复
		}
		m.sessions[r.ID] = Session{
			ID:        r.ID,
			Username:  r.Username,
			Role:      r.Role,
			CreatedAt: r.CreatedAt,
			ExpiresAt: r.ExpiresAt,
		}
		restored++
	}
	return nil
}

// SaveSessions 把未过期会话原子写入文件。
//
// 先写临时文件再 rename，避免读到半个 JSON；权限固定 0600。
func (m *SessionManager) SaveSessions(path string) error {
	if path == "" {
		return nil
	}

	now := m.now()

	m.mu.Lock()
	recs := make([]sessionRecord, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if !now.Before(sess.ExpiresAt) {
			continue
		}
		recs = append(recs, sessionRecord{
			ID:        sess.ID,
			Username:  sess.Username,
			Role:      sess.Role,
			CreatedAt: sess.CreatedAt,
			ExpiresAt: sess.ExpiresAt,
		})
	}
	m.mu.Unlock()

	raw, err := json.Marshal(recs)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
