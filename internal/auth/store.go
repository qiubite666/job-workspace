package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Role 是用户角色。admin 可增删改，viewer 只读。
type Role string

const (
	// RoleAdmin 拥有全部管理权限。
	RoleAdmin Role = "admin"
	// RoleViewer 仅有只读权限。
	RoleViewer Role = "viewer"
)

// fileVersion 是用户文件的格式版本号。
const fileVersion = 1

// 用户名规则：3~32 位，只允许小写字母、数字、点、下划线、连字符。
var (
	usernamePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)
	usernameMinLen  = 3
	usernameMaxLen  = 32

	passwordMinLen = 8
	passwordMaxLen = 128
)

// ParseRole 把字符串解析为 Role，忽略大小写与首尾空白；非法值返回 ErrInvalidRole。
func ParseRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleAdmin:
		return RoleAdmin, nil
	case RoleViewer:
		return RoleViewer, nil
	}
	return "", ErrInvalidRole
}

// User 是用户完整记录，PasswordHash 只在存储层与服务端校验时使用。
type User struct {
	Username     string `json:"username"`
	DisplayName  string `json:"displayName"`
	Role         Role   `json:"role"`
	PasswordHash string `json:"passwordHash"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	Disabled     bool   `json:"disabled"`
}

// PublicUser 是对外（API / 日志）暴露的用户视图，绝不含 PasswordHash。
type PublicUser struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        Role   `json:"role"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	Disabled    bool   `json:"disabled"`
}

// public 去掉敏感字段。
func (u User) public() PublicUser {
	return PublicUser{
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
		Disabled:    u.Disabled,
	}
}

// isActiveAdmin 表示该用户当前既具备管理员角色、又未被禁用。
func (u User) isActiveAdmin() bool {
	return u.Role == RoleAdmin && !u.Disabled
}

// userFile 是磁盘上的文件结构。
type userFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

// UserStore 是用户仓库，所有方法可并发调用。
//
// 内存中常驻全量用户；写操作先落盘再替换内存快照，避免「内存已改、磁盘失败」的不一致。
type UserStore struct {
	path   string
	mu     sync.RWMutex
	users  []User
	loaded bool
}

// NewUserStore 创建用户仓库；path 为用户文件路径，调用 Load 后才有数据。
//
// 即便忘记调用 Load，首次读写也会自动加载一次（见 ensureLoadedLocked），不会覆盖已有文件。
func NewUserStore(path string) *UserStore {
	return &UserStore{path: path, users: []User{}}
}

// Path 返回用户文件路径。
func (s *UserStore) Path() string { return s.path }

// Load 从磁盘加载用户文件。
//
// 文件不存在视为空集合，不报错；文件内容损坏（非法 JSON、版本不支持、空文件）返回带上下文的
// error，且保持内存状态不变——绝不静默清空，防止下一次写入覆盖真实用户数据。
func (s *UserStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Count 返回用户总数（含已禁用）。
func (s *UserStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// List 返回所有用户的公开视图，按 username 升序。
func (s *UserStore) List() []PublicUser {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]PublicUser, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u.public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// Get 按用户名返回完整记录（含哈希），仅供服务端内部校验使用。
func (s *UserStore) Get(username string) (User, bool) {
	name := strings.ToLower(strings.TrimSpace(username))

	s.mu.RLock()
	defer s.mu.RUnlock()

	if idx := s.indexOfLocked(name); idx >= 0 {
		return s.users[idx], true
	}
	return User{}, false
}

// Public 按用户名返回公开视图。
func (s *UserStore) Public(username string) (PublicUser, bool) {
	u, ok := s.Get(username)
	if !ok {
		return PublicUser{}, false
	}
	return u.public(), true
}

// Create 新建用户并落盘。
//
// 用户名会被 trim 并转小写；displayName 为空时回落为用户名。
// 错误：ErrUserExists（重名）、ErrWeakPassword（密码不合规）、ErrInvalidRole（角色非法），
// 用户名不合规时返回带说明的普通 error。
func (s *UserStore) Create(username, displayName string, role Role, password string) (PublicUser, error) {
	name, err := normalizeUsername(username)
	if err != nil {
		return PublicUser{}, err
	}
	if err := validatePassword(password); err != nil {
		return PublicUser{}, err
	}
	r, err := ParseRole(string(role))
	if err != nil {
		return PublicUser{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return PublicUser{}, fmt.Errorf("计算密码哈希失败: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return PublicUser{}, err
	}
	if s.indexOfLocked(name) >= 0 {
		return PublicUser{}, ErrUserExists
	}

	now := nowStamp()
	u := User{
		Username:     name,
		DisplayName:  displayNameOrDefault(displayName, name),
		Role:         r,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	next := append(cloneUsers(s.users), u)
	if err := s.saveLocked(next); err != nil {
		return PublicUser{}, err
	}
	s.users = next
	return u.public(), nil
}

// SetPassword 重置密码哈希并刷新 UpdatedAt。
//
// 返回 ErrUserNotFound 表示用户不存在；密码不合规返回 ErrWeakPassword。
// 会话清理由调用方负责（配合 SessionManager.DestroyUser）。
func (s *UserStore) SetPassword(username, password string) error {
	name, err := normalizeUsername(username)
	if err != nil {
		return ErrUserNotFound
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("计算密码哈希失败: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return err
	}
	idx := s.indexOfLocked(name)
	if idx < 0 {
		return ErrUserNotFound
	}

	next := cloneUsers(s.users)
	next[idx].PasswordHash = hash
	next[idx].UpdatedAt = nowStamp()
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.users = next
	return nil
}

// Update 局部更新用户；nil 字段表示不改。
//
// 只有在「把最后一个可用管理员降级为 viewer」或「禁用最后一个可用管理员」时返回 ErrLastAdmin。
func (s *UserStore) Update(username string, displayName *string, role *Role, disabled *bool) (PublicUser, error) {
	name, err := normalizeUsername(username)
	if err != nil {
		return PublicUser{}, ErrUserNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return PublicUser{}, err
	}
	idx := s.indexOfLocked(name)
	if idx < 0 {
		return PublicUser{}, ErrUserNotFound
	}

	next := cloneUsers(s.users)
	u := &next[idx]
	if displayName != nil {
		u.DisplayName = displayNameOrDefault(*displayName, u.Username)
	}
	if role != nil {
		r, err := ParseRole(string(*role))
		if err != nil {
			return PublicUser{}, err
		}
		u.Role = r
	}
	if disabled != nil {
		u.Disabled = *disabled
	}
	u.UpdatedAt = nowStamp()

	if err := checkLastAdmin(s.users, next); err != nil {
		return PublicUser{}, err
	}
	if err := s.saveLocked(next); err != nil {
		return PublicUser{}, err
	}
	s.users = next
	return u.public(), nil
}

// Delete 删除用户并落盘。
//
// 返回 ErrUserNotFound 表示用户不存在；删除后若再无可用管理员返回 ErrLastAdmin。
func (s *UserStore) Delete(username string) error {
	name, err := normalizeUsername(username)
	if err != nil {
		return ErrUserNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoadedLocked(); err != nil {
		return err
	}
	idx := s.indexOfLocked(name)
	if idx < 0 {
		return ErrUserNotFound
	}

	next := make([]User, 0, len(s.users)-1)
	next = append(next, s.users[:idx]...)
	next = append(next, s.users[idx+1:]...)

	if err := checkLastAdmin(s.users, next); err != nil {
		return err
	}
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.users = next
	return nil
}

// Verify 校验用户名与密码。
//
// 用户不存在与密码错误统一返回 ErrInvalidPassword（防止用户名枚举）；账号被禁用返回
// ErrUserDisabled。密码比对在锁外进行，避免 210000 轮计算阻塞其他读写。
func (s *UserStore) Verify(username, password string) (PublicUser, error) {
	name := strings.ToLower(strings.TrimSpace(username))

	s.mu.RLock()
	idx := s.indexOfLocked(name)
	if idx < 0 {
		s.mu.RUnlock()
		return PublicUser{}, ErrInvalidPassword
	}
	u := s.users[idx]
	s.mu.RUnlock()

	if u.Disabled {
		return PublicUser{}, ErrUserDisabled
	}
	if !VerifyPassword(u.PasswordHash, password) {
		return PublicUser{}, ErrInvalidPassword
	}
	return u.public(), nil
}

// ---------- 内部实现 ----------

// loadLocked 读盘并替换内存快照；调用方必须持有写锁。
func (s *UserStore) loadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.users = []User{}
			s.loaded = true
			return nil
		}
		return fmt.Errorf("读取用户文件 %s 失败: %w", s.path, err)
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("解析用户文件 %s 失败: 文件内容为空", s.path)
	}

	var f userFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析用户文件 %s 失败: %w", s.path, err)
	}
	if f.Version != 0 && f.Version != fileVersion {
		return fmt.Errorf("用户文件 %s 版本 %d 不受支持（期望 %d）", s.path, f.Version, fileVersion)
	}

	users := f.Users
	if users == nil {
		users = []User{}
	}
	s.users = users
	s.loaded = true
	return nil
}

// ensureLoadedLocked 保证内存快照来自磁盘；调用方必须持有写锁。
func (s *UserStore) ensureLoadedLocked() error {
	if s.loaded {
		return nil
	}
	return s.loadLocked()
}

// indexOfLocked 返回用户名下标，找不到返回 -1；调用方必须持锁。
func (s *UserStore) indexOfLocked(name string) int {
	for i := range s.users {
		if s.users[i].Username == name {
			return i
		}
	}
	return -1
}

// saveLocked 原子写入用户文件；调用方必须持有写锁。
func (s *UserStore) saveLocked(users []User) error {
	if users == nil {
		users = []User{}
	}
	data, err := json.MarshalIndent(userFile{Version: fileVersion, Users: users}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化用户数据失败: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("写入用户文件失败: %w", err)
	}
	return nil
}

// writeFileAtomic 原子写入：临时文件（0600）→ fsync → rename；目录权限 0700。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("落盘临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}

	// Windows 上 rename 不能覆盖已存在文件，先移除目标再重试。
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(path)
		if err2 := os.Rename(tmpName, path); err2 != nil {
			os.Remove(tmpName)
			return fmt.Errorf("替换 %s 失败: %w", path, err2)
		}
	}
	return nil
}

// normalizeUsername 规范化并校验用户名：trim + 小写 + 3~32 位 + 字符集。
func normalizeUsername(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if n := len(name); n < usernameMinLen || n > usernameMaxLen {
		return "", fmt.Errorf("用户名长度需为 %d~%d 位，实际 %d 位", usernameMinLen, usernameMaxLen, n)
	}
	if !usernamePattern.MatchString(name) {
		return "", errors.New("用户名只能包含小写字母、数字、点、下划线与连字符")
	}
	return name, nil
}

// validatePassword 校验密码：8~128 位（按字符计），且不能是纯空白。
func validatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < passwordMinLen || n > passwordMaxLen {
		return fmt.Errorf("%w: 长度需为 %d~%d 位，实际 %d 位", ErrWeakPassword, passwordMinLen, passwordMaxLen, n)
	}
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("%w: 不能全为空白字符", ErrWeakPassword)
	}
	return nil
}

// displayNameOrDefault 处理显示名：trim 后为空则回落到用户名。
func displayNameOrDefault(raw, fallback string) string {
	if name := strings.TrimSpace(raw); name != "" {
		return name
	}
	return fallback
}

// checkLastAdmin 比较变更前后是否把「可用管理员」清零。
//
// 原本就没有可用管理员时不做限制（例如初始化前只有 viewer 的文件），避免死锁式报错。
func checkLastAdmin(before, after []User) error {
	if !hasActiveAdmin(before) {
		return nil
	}
	if hasActiveAdmin(after) {
		return nil
	}
	return ErrLastAdmin
}

// hasActiveAdmin 判断集合中是否存在未禁用的管理员。
func hasActiveAdmin(users []User) bool {
	for _, u := range users {
		if u.isActiveAdmin() {
			return true
		}
	}
	return false
}

// cloneUsers 复制切片，保证「先落盘再替换快照」不会污染旧状态。
func cloneUsers(users []User) []User {
	out := make([]User, len(users))
	copy(out, users)
	return out
}

// nowStamp 返回 UTC 的 RFC3339 时间戳，统一存储格式便于排序与展示。
func nowStamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}
