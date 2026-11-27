// Package creds 管理每个账号的上报令牌与采集 cookie。
//
// 令牌和 cookie 都是账号级凭据：一个账号一份，互不可见。
// 文件落在 <dir>/<username>.json，权限 0600，只允许服务进程读取。
package creds

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 账号名即文件名，必须先校验，避免路径穿越。
var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

// ErrBadUsername 表示账号名不合法。
var ErrBadUsername = errors.New("账号名不合法")

// maxCookieLen 限制 cookie 体积，防止被当成廉价存储塞满磁盘。
const maxCookieLen = 16 << 10

// Credentials 是一个账号的凭据集合。
type Credentials struct {
	Token     string `json:"token"`            // 扩展上报令牌，48 位 hex
	Cookie    string `json:"cookie,omitempty"` // 采集用登录态；仅服务端可见，不回传前端
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// Store 是分账号凭据存储。
type Store struct {
	dir string
	mu  sync.RWMutex
	// index 是 token → 账号 的反查表。ingest 每个请求都要查，
	// 不能每次扫目录；写操作后会被置空并在下次查询时重建。
	index map[string]string
}

// New 创建凭据存储。
func New(dir string) *Store {
	return &Store{dir: dir, index: map[string]string{}}
}

// Dir 返回凭据目录。
func (s *Store) Dir() string { return s.dir }

func (s *Store) pathFor(username string) (string, error) {
	name := strings.TrimSpace(strings.ToLower(username))
	if !usernamePattern.MatchString(name) {
		return "", ErrBadUsername
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// Get 读取账号凭据；不存在时返回空结构而不是错误。
func (s *Store) Get(username string) (Credentials, error) {
	path, err := s.pathFor(username)
	if err != nil {
		return Credentials{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return readCreds(path)
}

// Ensure 返回账号的令牌，没有就生成一个。
func (s *Store) Ensure(username string) (string, error) {
	c, err := s.Get(username)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(c.Token) != "" {
		return c.Token, nil
	}
	return s.Rotate(username)
}

// Rotate 重新生成令牌（旧令牌立即失效）。
func (s *Store) Rotate(username string) (string, error) {
	path, err := s.pathFor(username)
	if err != nil {
		return "", err
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := readCreds(path)
	if err != nil {
		return "", err
	}
	c.Token = token
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := writeCreds(path, c); err != nil {
		return "", err
	}
	s.index = map[string]string{} // 失效，下次查重建
	return token, nil
}

// SetCookie 保存账号的采集 cookie；传空字符串等价于清除。
func (s *Store) SetCookie(username, cookie string) error {
	cookie = strings.TrimSpace(cookie)
	if len(cookie) > maxCookieLen {
		return errors.New("cookie 过长")
	}

	path, err := s.pathFor(username)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := readCreds(path)
	if err != nil {
		return err
	}
	c.Cookie = cookie
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if strings.TrimSpace(c.Token) == "" && cookie != "" {
		// 只填 cookie 时也保证有令牌，避免出现"有 cookie 但扩展用不了"的怪状态。
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		c.Token = hex.EncodeToString(buf)
	}
	s.index = map[string]string{}
	return writeCreds(path, c)
}

// ClearCookie 清除 cookie，保留令牌。
func (s *Store) ClearCookie(username string) error {
	return s.SetCookie(username, "")
}

// Resolve 用令牌反查账号。令牌无效时返回 false。
func (s *Store) Resolve(token string) (string, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}

	s.mu.Lock()
	index := s.index
	if len(index) == 0 {
		index = s.rebuildIndexLocked()
	}
	user, ok := index[token]
	s.mu.Unlock()

	return user, ok
}

// Remove 删除账号的全部凭据（账号被删除时调用）。
func (s *Store) Remove(username string) error {
	path, err := s.pathFor(username)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.index = map[string]string{}
	return nil
}

// AdoptLegacyToken 把旧的全局 ingest.token 接管为指定账号的令牌。
//
// 只有该账号还没有令牌时才接管，接管后旧文件改名为 .migrated，
// 这样已经在用的扩展配置不需要重新填写。
func (s *Store) AdoptLegacyToken(legacyPath, username string) (bool, error) {
	legacyPath = strings.TrimSpace(legacyPath)
	if legacyPath == "" {
		return false, nil
	}

	raw, err := os.ReadFile(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return false, nil
	}

	path, err := s.pathFor(username)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := readCreds(path)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(c.Token) != "" {
		return false, nil // 已有令牌，不覆盖
	}
	c.Token = token
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeCreds(path, c); err != nil {
		return false, err
	}
	s.index = map[string]string{}

	if err := os.Rename(legacyPath, legacyPath+".migrated"); err != nil {
		return true, nil
	}
	return true, nil
}

// rebuildIndexLocked 扫描目录重建 token → 账号 索引，调用方必须持锁。
func (s *Store) rebuildIndexLocked() map[string]string {
	index := map[string]string{}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.index = index
		return index
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		c, err := readCreds(filepath.Join(s.dir, e.Name()))
		if err != nil || strings.TrimSpace(c.Token) == "" {
			continue
		}
		// 同一个令牌只映射到一个账号；出现冲突时取文件名字典序最小者，
		// 保证结果稳定而不是随目录遍历顺序漂移。
		if prev, ok := index[c.Token]; ok && prev < name {
			continue
		}
		index[c.Token] = name
	}

	s.index = index
	return index
}

// readCreds 读取单个凭据文件；不存在返回空结构。
func readCreds(path string) (Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, nil
		}
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, err
	}
	return c, nil
}

// writeCreds 原子写入 0600 文件。
func writeCreds(path string, c Credentials) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".creds-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
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
