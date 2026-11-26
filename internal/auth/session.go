package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// sessionIDBytes 是会话 ID 的随机字节数（256 位熵，足以抵抗猜测）。
const sessionIDBytes = 32

// Session 是一次登录会话。ID 只下发给浏览器 Cookie，不落盘。
type Session struct {
	ID        string
	Username  string
	Role      Role
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionManager 是进程内会话表，所有方法可并发调用。
//
// 不持久化：进程重启后全部失效，单实例部署可接受。多实例部署需要换成共享存储。
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]Session
	ttl      time.Duration
	now      func() time.Time // 测试可注入时钟，生产固定 time.Now
}

// NewSessionManager 创建会话管理器；ttl <= 0 时回落到 12 小时默认值。
func NewSessionManager(ttl time.Duration) *SessionManager {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionManager{
		sessions: make(map[string]Session),
		ttl:      ttl,
		now:      time.Now,
	}
}

// TTL 返回会话有效期。
func (m *SessionManager) TTL() time.Duration { return m.ttl }

// Create 为用户生成新会话：ID 为 32 字节 crypto/rand 的 base64url（无填充）编码。
func (m *SessionManager) Create(u PublicUser) (Session, error) {
	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	sess := Session{
		ID:        base64.RawURLEncoding.EncodeToString(raw),
		Username:  u.Username,
		Role:      u.Role,
		CreatedAt: now,
		ExpiresAt: now.Add(m.ttl),
	}
	m.sessions[sess.ID] = sess
	return sess, nil
}

// Get 按 ID 取会话。
//
// 已过期则删除并返回 false；剩余有效期不足 ttl/2 时滑动续期（ExpiresAt = now + ttl）。
func (m *SessionManager) Get(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[id]
	if !ok {
		return Session{}, false
	}

	now := m.now()
	if !now.Before(sess.ExpiresAt) {
		delete(m.sessions, id)
		return Session{}, false
	}

	if sess.ExpiresAt.Sub(now) < m.ttl/2 {
		sess.ExpiresAt = now.Add(m.ttl)
		m.sessions[id] = sess
	}
	return sess, true
}

// Destroy 销毁单个会话；不存在时静默忽略（登出需要幂等）。
func (m *SessionManager) Destroy(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// DestroyUser 清理某用户的全部会话，返回被清理数量。
//
// 用于改密码、禁用、删除用户后把该用户踢下线。
func (m *SessionManager) DestroyUser(username string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for id, sess := range m.sessions {
		if sess.Username == username {
			delete(m.sessions, id)
			n++
		}
	}
	return n
}

// Purge 清理全部已过期会话。
func (m *SessionManager) Purge() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	for id, sess := range m.sessions {
		if !now.Before(sess.ExpiresAt) {
			delete(m.sessions, id)
		}
	}
}

// Len 返回当前有效（未过期）的会话数；已过期但尚未清理的不计入。
func (m *SessionManager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	n := 0
	for _, sess := range m.sessions {
		if now.Before(sess.ExpiresAt) {
			n++
		}
	}
	return n
}
