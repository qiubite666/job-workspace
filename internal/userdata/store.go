// Package userdata 把用户的收藏、投递阶段与备注持久化到本地磁盘。
//
// 每个用户一个 JSON 文件：<dir>/<username>.json，文件权限 0600、目录 0700。
// 写入采用「临时文件 → fsync → rename」的原子替换，避免进程中途退出留下半截文件；
// 读写在进程内由 sync.RWMutex 串行化，可安全地被多个 HTTP 请求并发调用。
package userdata

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

// fileVersion 是用户状态文件的格式版本号，对应 contracts/V2.md §2.1 的 version 字段。
const fileVersion = 1

// 字段硬上限，对应 contracts/V2.md §2.1：
// 收藏最多 5000 条，stages / notes 最多 5000 键，单条备注最多 500 字符（按 rune 计）。
const (
	maxFavorites = 5000
	maxStageKeys = 5000
	maxNoteKeys  = 5000
	maxNoteRunes = 500
)

// 用户名规则与 internal/auth 保持一致：3~32 位，只允许小写字母、数字、点、下划线、连字符。
const (
	usernameMinLen = 3
	usernameMaxLen = 32
)

// usernamePattern 与 auth 包的同名规则逐字一致，保证「能登录的账号就有状态文件」。
var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// allowedStages 是投递阶段白名单；空串表示未投递，同样合法。
var allowedStages = map[string]struct{}{
	"":          {},
	"applied":   {},
	"interview": {},
	"offer":     {},
	"rejected":  {},
}

// ErrInvalidUsername 表示用户名未通过规范化校验（空、超长、含路径分隔符、字符集非法等）。
//
// 它是路径穿越防护的对外信号：调用方拿到该错误时绝不会产生任何文件读写。
var ErrInvalidUsername = errors.New("用户名不合法")

// errEmptyDir 表示仓库目录未配置；空目录会把文件写到进程当前目录，因此直接拒绝。
var errEmptyDir = errors.New("用户状态目录未配置")

// State 是单个用户的云端状态，形状与 contracts/V2.md §2.1 一致。
//
// 除 NewState 与 Store 的返回值外，调用方传入的 State 不需要预先归一化：
// Save 会补齐 Version、重算 UpdatedAt，并清理重复与越界数据。
type State struct {
	Version   int               `json:"version"`
	Favorites []string          `json:"favorites"`
	Stages    map[string]string `json:"stages"`
	Notes     map[string]string `json:"notes"`
	UpdatedAt string            `json:"updatedAt"`
}

// NewState 返回空状态：版本 1、非 nil 的空切片与空映射。
//
// 容器永远不为 nil，前端可以直接遍历、JSON 里也不会出现 null。
func NewState() State {
	return State{
		Version:   fileVersion,
		Favorites: []string{},
		Stages:    map[string]string{},
		Notes:     map[string]string{},
	}
}

// Store 是用户状态的磁盘仓库，所有方法均可并发调用。
type Store struct {
	dir string
	mu  sync.RWMutex
}

// NewStore 创建仓库；dir 形如 <data>/userstate，目录在首次写入时按 0700 创建。
func NewStore(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
}

// Path 返回用户名对应的文件路径；用户名不合法时返回空字符串。
//
// 安全性：用户名先按 auth 同款规则规范化（trim + 小写 + [a-z0-9._-]{3,32}），
// 任何含 `..`、`/`、`\` 的输入在字符集校验阶段即被拒绝，因此拼出的路径不可能跳出 dir。
func (s *Store) Path(username string) string {
	path, err := s.pathFor(username)
	if err != nil {
		return ""
	}
	return path
}

// Load 读取用户状态。
//
// 文件不存在视为「尚无数据」，返回空 State 与 nil error，不是错误；
// 文件损坏（空文件、非法 JSON、版本不支持）返回带路径上下文的 error，
// 绝不静默返回空状态——否则调用方一次保存就会覆盖掉真实数据。
func (s *Store) Load(username string) (State, error) {
	path, err := s.pathFor(username)
	if err != nil {
		return State{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewState(), nil
		}
		return State{}, fmt.Errorf("读取用户状态文件 %s 失败: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return State{}, fmt.Errorf("解析用户状态文件 %s 失败: 文件内容为空", path)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("解析用户状态文件 %s 失败: %w", path, err)
	}
	if st.Version != 0 && st.Version != fileVersion {
		return State{}, fmt.Errorf("用户状态文件 %s 版本 %d 不受支持（期望 %d）", path, st.Version, fileVersion)
	}

	// 读出的数据同样过一遍归一化：文件可能被手工改坏或来自旧版本，
	// 保证调用方拿到的字段形状始终合法。归一化不重算 UpdatedAt。
	return normalizeState(st), nil
}

// Save 归一化并原子写入用户状态，返回真正落盘的内容。
//
// 归一化规则（契约 §2.1）：收藏去空白、去重、截断到 5000 条；阶段非法值一律归为 "";
// 备注去空白、按 rune 截断到 500、超 5000 键按键名排序截断；Version 固定为 1，
// UpdatedAt 由服务端重算（UTC RFC3339），调用方传入的这两个字段会被覆盖。
func (s *Store) Save(username string, st State) (State, error) {
	path, err := s.pathFor(username)
	if err != nil {
		return State{}, err
	}

	next := normalizeState(st)
	next.Version = fileVersion
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return State{}, fmt.Errorf("序列化用户状态失败: %w", err)
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFileAtomic(path, data); err != nil {
		return State{}, err
	}
	return next, nil
}

// Delete 删除用户状态文件；文件不存在也返回 nil（幂等）。
func (s *Store) Delete(username string) error {
	path, err := s.pathFor(username)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除用户状态文件 %s 失败: %w", path, err)
	}
	return nil
}

// ---------- 内部实现 ----------

// pathFor 规范化用户名并拼出文件路径；这是唯一的路径构造入口。
func (s *Store) pathFor(username string) (string, error) {
	if s.dir == "" {
		return "", errEmptyDir
	}
	name, err := normalizeUsername(username)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// normalizeUsername 规范化并校验用户名：trim + 小写 + 字符集 + 长度。
//
// 显式拒绝路径分隔符与「纯点」名称，即使正则已经覆盖，也不让路径穿越依赖单一防线。
func normalizeUsername(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return "", fmt.Errorf("%w: 不能为空", ErrInvalidUsername)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: 不能包含路径分隔符", ErrInvalidUsername)
	}
	if n := len(name); n < usernameMinLen || n > usernameMaxLen {
		return "", fmt.Errorf("%w: 长度需为 %d~%d 位，实际 %d 位", ErrInvalidUsername, usernameMinLen, usernameMaxLen, n)
	}
	if !usernamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: 只能包含小写字母、数字、点、下划线与连字符", ErrInvalidUsername)
	}
	if strings.Trim(name, ".") == "" {
		return "", fmt.Errorf("%w: 不能只由点组成", ErrInvalidUsername)
	}
	return name, nil
}

// normalizeState 归一化全部字段；不含 UpdatedAt 的重算。
func normalizeState(st State) State {
	return State{
		Version:   fileVersion,
		Favorites: normalizeFavorites(st.Favorites),
		Stages:    normalizeStages(st.Stages),
		Notes:     normalizeNotes(st.Notes),
		UpdatedAt: st.UpdatedAt,
	}
}

// normalizeFavorites 去空白、去空串、去重并按原始顺序截断到上限。
func normalizeFavorites(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= maxFavorites {
			break
		}
	}
	return out
}

// normalizeStages 清理键并白名单化阶段值；非法值归为 ""（未投递）。
func normalizeStages(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for rawID, rawStage := range in {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		stage := strings.ToLower(strings.TrimSpace(rawStage))
		if _, ok := allowedStages[stage]; !ok {
			stage = ""
		}
		out[id] = stage
	}
	return capStringMap(out, maxStageKeys)
}

// normalizeNotes 清理键、丢弃空备注、按 rune 截断单条长度。
func normalizeNotes(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for rawID, rawNote := range in {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		note := strings.TrimSpace(rawNote)
		if note == "" {
			continue
		}
		out[id] = truncateRunes(note, maxNoteRunes)
	}
	return capStringMap(out, maxNoteKeys)
}

// capStringMap 在超限时按键名升序保留前 max 个键，保证截断结果可复现
// （map 遍历顺序随机，若不排序，同样的输入会写出不同的文件）。
func capStringMap(in map[string]string, max int) map[string]string {
	if len(in) <= max {
		return in
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, max)
	for _, k := range keys[:max] {
		out[k] = in[k]
	}
	return out
}

// truncateRunes 按字符（rune）截断，避免切断多字节汉字。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// writeFileAtomic 原子写入：目录 0700，临时文件 0600 → fsync → rename。
//
// 只有 rename 成功新内容才对外可见，读端永远不会看到写了一半的 JSON。
func writeFileAtomic(path string, data []byte) (err error) {
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
