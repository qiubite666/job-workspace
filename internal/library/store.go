// Package library 按账号存放职位库。
//
// 每个账号一个文件：<dir>/<username>.json。账号之间完全隔离，
// 读取、合并、清空都必须带上 username，不存在"全局库"这个概念。
package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"jobworkspace/internal/model"
)

// 账号名即文件名，必须先校验，否则 ../ 之类的输入会写到目录外。
var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

// ErrBadUsername 表示账号名不合法（可能尝试路径穿越）。
var ErrBadUsername = errors.New("账号名不合法")

// Store 是分账号职位库。
type Store struct {
	dir string
	mu  sync.RWMutex
}

// New 创建存储；dir 不存在时首次写入会自动创建。
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Dir 返回库文件所在目录。
func (s *Store) Dir() string { return s.dir }

// pathFor 计算账号的库文件路径，并做账号名校验。
func (s *Store) pathFor(username string) (string, error) {
	name := strings.TrimSpace(strings.ToLower(username))
	if !usernamePattern.MatchString(name) {
		return "", ErrBadUsername
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// Load 读取指定账号的职位库；文件不存在时返回空切片而不是错误。
func (s *Store) Load(username string) ([]model.Job, string, error) {
	path, err := s.pathFor(username)
	if err != nil {
		return nil, "", err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []model.Job{}, "", nil
		}
		return nil, "", err
	}

	var jobs []model.Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, "", fmt.Errorf("解析 %s 失败: %w", filepath.Base(path), err)
	}
	if jobs == nil {
		jobs = []model.Job{}
	}
	return jobs, modifiedAt(path), nil
}

// Save 覆盖写入指定账号的职位库。
func (s *Store) Save(username string, jobs []model.Job) error {
	path, err := s.pathFor(username)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return writeJobs(path, jobs)
}

// Merge 把列表包并入指定账号的职位库，返回新增数与总数。
func (s *Store) Merge(username string, incoming []model.Job) (added, total int, err error) {
	return s.mergeInto(username, incoming, false)
}

// UpsertDetails 把详情包并入指定账号的职位库。
func (s *Store) UpsertDetails(username string, incoming []model.Job) (updated, added, total int, err error) {
	path, err := s.pathFor(username)
	if err != nil {
		return 0, 0, 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := readJobs(path)
	if err != nil {
		return 0, 0, 0, err
	}

	merged := make([]model.Job, 0, len(existing)+len(incoming))
	index := make(map[string]int, len(existing)+len(incoming))
	for _, j := range existing {
		if _, dup := index[j.ID]; dup {
			continue
		}
		index[j.ID] = len(merged)
		merged = append(merged, j)
	}

	for _, j := range incoming {
		if pos, ok := index[j.ID]; ok {
			merged[pos] = model.MergeDetailFields(merged[pos], j)
			updated++
			continue
		}
		index[j.ID] = len(merged)
		merged = append(merged, j)
		added++
	}

	if err := writeJobs(path, merged); err != nil {
		return 0, 0, 0, err
	}
	return updated, added, len(merged), nil
}

// Clear 清空指定账号的职位库，返回被清掉的条数。
//
// 只动机该账号自己的文件：即使文件不存在也返回 0，不报错。
func (s *Store) Clear(username string) (removed int, err error) {
	path, err := s.pathFor(username)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	jobs, err := readJobs(path)
	if err != nil {
		return 0, err
	}
	if err := writeJobs(path, []model.Job{}); err != nil {
		return 0, err
	}
	return len(jobs), nil
}

// AdoptLegacy 把旧的全局 jobs.json 迁移给指定账号。
//
// 仅在目标账号库为空、且旧文件存在时执行一次；迁移后旧文件改名为 .migrated，
// 保留只读副本便于回滚，同时避免下次启动重复迁移。
func (s *Store) AdoptLegacy(globalPath, username string) (int, error) {
	if strings.TrimSpace(globalPath) == "" {
		return 0, nil
	}
	if _, err := s.pathFor(username); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	legacy, err := readJobs(globalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if len(legacy) == 0 {
		return 0, nil
	}

	target, _ := s.pathFor(username)
	if cur, _ := readJobs(target); len(cur) > 0 {
		return 0, nil // 已有数据，不覆盖
	}

	if err := writeJobs(target, legacy); err != nil {
		return 0, err
	}
	// 改名而非删除：迁移出错时还能找回。
	if err := os.Rename(globalPath, globalPath+".migrated"); err != nil {
		return len(legacy), nil
	}
	return len(legacy), nil
}

// mergeInto 是 Merge 的实现，detail 为 false 时走列表合并语义。
func (s *Store) mergeInto(username string, incoming []model.Job, detail bool) (int, int, error) {
	path, err := s.pathFor(username)
	if err != nil {
		return 0, 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := readJobs(path)
	if err != nil {
		return 0, 0, err
	}

	merged := make([]model.Job, 0, len(existing)+len(incoming))
	index := make(map[string]int, len(existing)+len(incoming))
	for _, j := range existing {
		if _, dup := index[j.ID]; dup {
			continue
		}
		index[j.ID] = len(merged)
		merged = append(merged, j)
	}

	added := 0
	for _, j := range incoming {
		if pos, ok := index[j.ID]; ok {
			merged[pos] = model.MergePreserving(merged[pos], j)
			continue
		}
		index[j.ID] = len(merged)
		merged = append(merged, j)
		added++
	}

	if err := writeJobs(path, merged); err != nil {
		return 0, 0, err
	}
	return added, len(merged), nil
}

// readJobs 读取库文件；不存在视为空库。
func readJobs(path string) ([]model.Job, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []model.Job{}, nil
		}
		return nil, err
	}
	var jobs []model.Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", filepath.Base(path), err)
	}
	if jobs == nil {
		jobs = []model.Job{}
	}
	return jobs, nil
}

// writeJobs 原子写入：先写临时文件再 rename，避免读到半个 JSON。
func writeJobs(path string, jobs []model.Job) error {
	if jobs == nil {
		jobs = []model.Job{}
	}
	raw, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".jobs-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
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

// modifiedAt 返回文件修改时间（RFC3339），取不到时返回空串。
func modifiedAt(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return info.ModTime().Format(time.RFC3339)
}
