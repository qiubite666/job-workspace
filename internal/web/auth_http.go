package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"jobworkspace/internal/auth"
)

// sessionCookieName 是会话 Cookie 名。
const sessionCookieName = "wb_sid"

// loginBodyLimit 是登录请求体上限（64KB）。
const loginBodyLimit = 64 << 10

// 登录限流参数：同一 (客户端IP, 用户名) 连续失败 maxLoginFailures 次后
// 锁定 loginLockDuration。
const (
	maxLoginFailures  = 5
	loginLockDuration = 60 * time.Second
	loginGuardTTL     = 10 * time.Minute
)

// loginGuard 是按 (IP, 用户名) 分桶的内存失败计数器。
//
// 仅为降低暴力破解成本（单实例、本地部署），因此不加持久化。
type loginGuard struct {
	mu        sync.Mutex
	buckets   map[string]*loginBucket
	lastPurge time.Time
}

// loginBucket 是单个桶的状态。
type loginBucket struct {
	fails     int
	lockedTo  time.Time
	touchedAt time.Time
}

// newLoginGuard 构造一个空限流器。
func newLoginGuard() *loginGuard {
	return &loginGuard{buckets: make(map[string]*loginBucket)}
}

// loginLimiter 是包级共享实例：登录限流不需要随 Server 实例变化。
var loginLimiter = newLoginGuard()

// loginGuardKey 生成限流桶键。
func loginGuardKey(ip, username string) string {
	return ip + "\x00" + strings.ToLower(strings.TrimSpace(username))
}

// locked 判断该 (IP, 用户名) 当前是否处于锁定期，返回剩余锁定时长。
func (g *loginGuard) locked(key string, now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	b, ok := g.buckets[key]
	if !ok {
		return 0, false
	}
	if now.Before(b.lockedTo) {
		return b.lockedTo.Sub(now), true
	}
	return 0, false
}

// fail 记录一次失败；达到阈值时开始锁定（从下一次请求起生效）。
func (g *loginGuard) fail(key string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.purgeLocked(now)

	b, ok := g.buckets[key]
	if !ok {
		b = &loginBucket{}
		g.buckets[key] = b
	}
	b.fails++
	b.touchedAt = now
	if b.fails >= maxLoginFailures {
		b.lockedTo = now.Add(loginLockDuration)
		b.fails = 0
	}
}

// reset 在登录成功后清空该桶。
func (g *loginGuard) reset(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.buckets, key)
}

// purgeLocked 清理长期未活动且未处于锁定期的桶，调用方必须已持锁。
func (g *loginGuard) purgeLocked(now time.Time) {
	if now.Sub(g.lastPurge) < time.Minute {
		return
	}
	g.lastPurge = now
	for key, b := range g.buckets {
		if now.Before(b.lockedTo) {
			continue
		}
		if now.Sub(b.touchedAt) > loginGuardTTL {
			delete(g.buckets, key)
		}
	}
}

// loginRequest 是 POST /api/auth/login 的请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 处理登录：校验凭据、建立会话并下发 Cookie。
//
// 成功 200 {"ok":true,"user":PublicUser}；凭据错误 401 INVALID_CREDENTIAL；
// 连续失败过多 429 TOO_MANY_ATTEMPTS；非 JSON 请求体 415。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "请求体必须是 application/json")
		return
	}

	var req loginRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON")
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIAL", "用户名或密码不正确")
		return
	}

	ip := clientIP(r)
	key := loginGuardKey(ip, username)
	now := time.Now()
	if _, locked := loginLimiter.locked(key, now); locked {
		writeError(w, http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "登录失败次数过多，请稍后再试")
		return
	}

	if s.users == nil || s.sessions == nil {
		s.log.Error("登录失败：认证组件未初始化")
		writeError(w, http.StatusInternalServerError, "AUTH_UNAVAILABLE", "登录服务不可用")
		return
	}

	// 一个账号都没有时，任何密码都不可能正确。此时直接告知「未初始化」，
	// 既不消耗失败次数（否则会把用户锁进 429），也避免误导用户以为密码打错了。
	if s.users.Count() == 0 {
		writeError(w, http.StatusUnauthorized, "AUTH_NOT_INITIALIZED", "后台尚未初始化，请先在服务器上创建管理员账号")
		return
	}

	user, err := s.users.Verify(username, req.Password)
	if err != nil {
		loginLimiter.fail(key, now)
		// 统一话术，避免通过错误差异枚举用户。
		s.log.Info("登录失败", "ip", ip, "username", username)
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIAL", "用户名或密码不正确")
		return
	}

	sess, err := s.sessions.Create(user)
	if err != nil {
		s.log.Error("创建会话失败", "err", err)
		writeError(w, http.StatusInternalServerError, "SESSION_FAILED", "创建会话失败")
		return
	}

	loginLimiter.reset(key)
	s.setSessionCookie(w, r, sess)
	s.saveSessions()
	s.log.Info("登录成功", "ip", ip, "username", user.Username, "role", string(user.Role))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"user": user,
	})
}

// handleLogout 销毁当前会话并清除 Cookie；未登录也返回 200（幂等）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.sessions != nil {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			s.sessions.Destroy(c.Value)
		}
	}
	s.saveSessions()
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMe 返回当前登录状态；**永不返回 401**，供前端启动时探测。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	_, user, state := s.authenticate(r)
	switch state {
	case authStateOK:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"authenticated": true,
			"user":          user,
		})
	case authStateNoUsers:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"authenticated": false,
			"error":         "AUTH_NOT_INITIALIZED",
			"message":       "后台尚未初始化",
		})
	default:
		// 会话失效或账号已被禁用/删除：顺手清掉残留 Cookie。
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			s.clearSessionCookie(w, r)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"authenticated": false,
		})
	}
}

// requireAuth 要求已登录：API 路径回 401 JSON，页面路径 302 到 /admin。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, state := s.authenticate(r); state != authStateOK {
			rejectUnauthenticated(w, r, state)
			return
		}
		next(w, r)
	}
}

// requireRole 先认证再校验角色；角色取 authenticate 复核后的最新值，
// 因此「降级为 viewer」对已登录会话立即生效。
func (s *Server) requireRole(role auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, _, state := s.authenticate(r)
		if state != authStateOK {
			rejectUnauthenticated(w, r, state)
			return
		}
		if sess.Role != role {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "没有权限")
			return
		}
		next(w, r)
	}
}

// authState 是一次认证复核的结果。
type authState int

const (
	// authStateOK 表示会话有效且账号当前可用。
	authStateOK authState = iota
	// authStateAnon 表示未登录、会话过期，或账号已被禁用/删除。
	authStateAnon
	// authStateNoUsers 表示用户存储未初始化（数据目录缺失或加载失败）。
	authStateNoUsers
)

// authenticate 取出会话并**每请求复核用户当前状态**。
//
// 不信任会话里的角色快照：用户被删除或禁用时立即销毁会话拒绝放行，
// 角色变更时以用户库为准覆盖 sess.Role，使降级/升级即时生效。
// authenticate 解析请求身份，并**每次**回查用户库确认账号仍然可用。
//
// 返回 state 用于区分三种拒绝原因：未初始化（一个账号都没有）、未登录、正常。
func (s *Server) authenticate(r *http.Request) (auth.Session, auth.PublicUser, authState) {
	if s.sessions == nil || s.users == nil {
		return auth.Session{}, auth.PublicUser{}, authStateNoUsers
	}
	// 「有用户存储但一个账号都没有」同样属于未初始化：
	// 生产环境里 store 永远非 nil，所以只判断 users==nil 会让首次部署
	// 落进「密码错误」分支，把用户引向反复试错。
	if s.users.Count() == 0 {
		return auth.Session{}, auth.PublicUser{}, authStateNoUsers
	}

	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return auth.Session{}, auth.PublicUser{}, authStateAnon
	}
	sess, ok := s.sessions.Get(c.Value)
	if !ok {
		return auth.Session{}, auth.PublicUser{}, authStateAnon
	}

	user, found := s.users.Public(sess.Username)
	if !found || user.Disabled {
		s.sessions.Destroy(sess.ID)
		s.saveSessions()
		return auth.Session{}, auth.PublicUser{}, authStateAnon
	}

	sess.Role = user.Role
	return sess, user, authStateOK
}

// rejectUnauthenticated 按请求类型给出 401 JSON 或 302 跳转。
func rejectUnauthenticated(w http.ResponseWriter, r *http.Request, state authState) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	if state == authStateNoUsers {
		writeError(w, http.StatusUnauthorized, "AUTH_NOT_INITIALIZED", "后台尚未初始化")
		return
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
}

// currentSession 取出并复核当前请求的会话，仅关心「是否已登录」时使用。
func (s *Server) currentSession(r *http.Request) (auth.Session, bool) {
	sess, _, state := s.authenticate(r)
	return sess, state == authStateOK
}

// sessionMaxAge 返回 Cookie 的 MaxAge（秒），直接取会话管理器的有效期，
// 避免与 web.New 里创建 SessionManager 时传入的 TTL 出现偏差。
func (s *Server) sessionMaxAge() int {
	if s.sessions != nil {
		if ttl := s.sessions.TTL(); ttl > 0 {
			return int(ttl.Seconds())
		}
	}
	return int(sessionTTL.Seconds())
}

// setSessionCookie 下发会话 Cookie。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		MaxAge:   s.sessionMaxAge(),
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie 立刻使 Cookie 失效。
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// isSecureRequest 判断是否 HTTPS：直连看 TLS，反代看 X-Forwarded-Proto。
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// clientIP 取客户端 IP：优先 X-Forwarded-For 首段，否则 RemoteAddr 的 host 部分。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
