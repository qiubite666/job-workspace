// Package store 负责采集数据的本地持久化。
//
// 只做三件事：读、写、按 ID 合并。文件布局固定：
//   - jobs.json            采集结果（写入目标，也是唯一数据来源）
//   - collect.report.json  最近一次采集报告
//
// 写入统一走「临时文件 + rename」，避免进程中断留下半截 JSON。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"jobworkspace/internal/model"
)

// 文件名常量，供外部（如 web 层）判断数据来源。
const (
	// JobsFile 是采集结果文件名。
	JobsFile = "jobs.json"
	// ReportFile 是采集报告文件名。
	ReportFile = "collect.report.json"

	// SourceCollected 表示数据来自真实采集。
	SourceCollected = "collected"
	// SourceEmpty 表示没有任何数据。
	SourceEmpty = "empty"
)

// Store 是本地数据仓库，所有方法可并发调用。
type Store struct {
	mu  sync.Mutex
	dir string
}

// New 创建 Store；dataDir 为空时默认 "data"。
func New(dataDir string) *Store {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		dir = "data"
	}
	return &Store{dir: dir}
}

// Dir 返回实际使用的数据目录。
func (s *Store) Dir() string { return s.dir }

// LoadJobs 读取岗位数据。
//
// jobs.json 有内容 → source=collected；文件不存在或为空 → (nil, "empty", nil)，
// 由前端展示空态。文件存在但 JSON 损坏时返回错误。
func (s *Store) LoadJobs() ([]model.Job, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	jobs, ok, err := readJobsFile(s.jobsPath())
	if err != nil {
		return nil, SourceEmpty, err
	}
	if ok {
		return jobs, SourceCollected, nil
	}
	return nil, SourceEmpty, nil
}

// SaveJobs 覆盖写入 jobs.json。
func (s *Store) SaveJobs(jobs []model.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveJobsLocked(jobs)
}

// MergeAndSave 按 ID 合并 incoming 与已有数据，并写回 jobs.json。
//
// 同一 ID 用 incoming 覆盖（含新的 CollectedAt）；新增数 = 原来不存在的 ID 数。
// 已有记录的相对顺序保持不变，新记录追加在末尾。
func (s *Store) MergeAndSave(incoming []model.Job) (added, total int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, _, rErr := readJobsFile(s.jobsPath())
	if rErr != nil {
		return 0, 0, rErr
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
			merged[pos] = mergePreserving(merged[pos], j)
			continue
		}
		index[j.ID] = len(merged)
		merged = append(merged, j)
		added++
	}

	if err := s.saveJobsLocked(merged); err != nil {
		return 0, 0, err
	}
	return added, len(merged), nil
}

// SaveReport 覆盖写入 collect.report.json。
func (s *Store) SaveReport(r model.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.reportPath(), r)
}

// LoadReport 读取最近一次采集报告；不存在或损坏时返回 false。
func (s *Store) LoadReport() (model.Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.reportPath())
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return model.Report{}, false
	}
	var r model.Report
	if err := json.Unmarshal(data, &r); err != nil {
		return model.Report{}, false
	}
	return r, true
}

// ---------- 内部实现 ----------

func (s *Store) jobsPath() string   { return filepath.Join(s.dir, JobsFile) }
func (s *Store) reportPath() string { return filepath.Join(s.dir, ReportFile) }

// saveJobsLocked 写入岗位数据；空切片也写成 []，避免下游拿到 null。
func (s *Store) saveJobsLocked(jobs []model.Job) error {
	if jobs == nil {
		jobs = []model.Job{}
	}
	return writeJSON(s.jobsPath(), jobs)
}

// readJobsFile 读取岗位数组；文件不存在或内容为空时 ok=false 且不报错。
func readJobsFile(path string) ([]model.Job, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, false, nil
	}
	var jobs []model.Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, false, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if len(jobs) == 0 {
		return nil, false, nil
	}
	return jobs, true, nil
}

// writeJSON 原子写入：同目录临时文件 → fsync → rename。
//
// 缩进两空格、不转义 HTML，保证生成的 JSON 可直接进版本库与前端。
func writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		cleanup()
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("落盘 %s 失败: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭 %s 失败: %w", path, err)
	}
	// Windows 上 rename 不能覆盖已存在文件，先移除目标。
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(path)
		if err2 := os.Rename(tmpName, path); err2 != nil {
			os.Remove(tmpName)
			return fmt.Errorf("替换 %s 失败: %w", path, err2)
		}
	}
	return nil
}

// mergePreserving 用 incoming 覆盖 existing，但 incoming 的空值不抹掉已有内容。
//
// 列表接口不返回职位描述，如果直接整条替换，用户点开详情补全的 JD 会在下一次
// 列表上报时被清空。这里按字段做「有则覆盖、无则保留」的合并。
func mergePreserving(existing, incoming model.Job) model.Job {
	out := incoming

	if strings.TrimSpace(out.Description) == "" {
		out.Description = existing.Description
	}
	if strings.TrimSpace(out.ActiveTimeDesc) == "" {
		out.ActiveTimeDesc = existing.ActiveTimeDesc
	}
	if strings.TrimSpace(out.PublishedAt) == "" {
		out.PublishedAt = existing.PublishedAt
		out.PublishedAtSource = existing.PublishedAtSource
	}
	if strings.TrimSpace(out.JobURL) == "" {
		out.JobURL = existing.JobURL
	}
	if strings.TrimSpace(out.BossName) == "" {
		out.BossName = existing.BossName
	}
	if strings.TrimSpace(out.BossTitle) == "" {
		out.BossTitle = existing.BossTitle
	}
	if strings.TrimSpace(out.CityName) == "" {
		out.CityName = existing.CityName
	}
	if strings.TrimSpace(out.AreaDistrict) == "" {
		out.AreaDistrict = existing.AreaDistrict
	}
	if strings.TrimSpace(out.BusinessDistrict) == "" {
		out.BusinessDistrict = existing.BusinessDistrict
	}
	if strings.TrimSpace(out.Location) == "" {
		out.Location = existing.Location
	}
	if strings.TrimSpace(out.SalaryDesc) == "" {
		out.SalaryDesc = existing.SalaryDesc
		out.SalaryMin, out.SalaryMax, out.SalaryMonths = existing.SalaryMin, existing.SalaryMax, existing.SalaryMonths
		out.SalaryUnit, out.SalaryKind = existing.SalaryUnit, existing.SalaryKind
		out.SalaryMonthlyMin, out.SalaryMonthlyMax = existing.SalaryMonthlyMin, existing.SalaryMonthlyMax
		out.SalaryText = existing.SalaryText
	}
	if strings.TrimSpace(out.Experience) == "" {
		out.Experience = existing.Experience
	}
	if strings.TrimSpace(out.Degree) == "" {
		out.Degree = existing.Degree
	}
	if strings.TrimSpace(out.BrandIndustry) == "" {
		out.BrandIndustry = existing.BrandIndustry
	}
	if strings.TrimSpace(out.BrandScaleName) == "" {
		out.BrandScaleName = existing.BrandScaleName
	}
	if strings.TrimSpace(out.BrandStageName) == "" {
		out.BrandStageName = existing.BrandStageName
	}
	if len(out.Skills) == 0 {
		out.Skills = existing.Skills
	}
	if len(out.JobLabels) == 0 {
		out.JobLabels = existing.JobLabels
	}
	if len(out.WelfareList) == 0 {
		out.WelfareList = existing.WelfareList
	}
	return out
}

// UpsertDetails 合并详情包：按 ID 命中则补全字段，未命中则新增。
//
// 详情包字段少（只有 JD、招聘者、活跃度等），因此合并方向是
// 「已有记录为底，用详情里的非空值覆盖」，不能反过来，否则会把列表带的
// 薪资、标签等字段清空。
func (s *Store) UpsertDetails(incoming []model.Job) (updated, added, total int, err error) {
	if len(incoming) == 0 {
		return 0, 0, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, _, rErr := readJobsFile(s.jobsPath())
	if rErr != nil {
		return 0, 0, 0, rErr
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
			// 只覆盖详情专属字段，其余沿用库里（不能反向整条覆盖）。
			merged[pos] = mergeDetailFields(merged[pos], j)
			updated++
			continue
		}
		index[j.ID] = len(merged)
		merged = append(merged, j)
		added++
	}

	if err := s.saveJobsLocked(merged); err != nil {
		return 0, 0, 0, err
	}
	return updated, added, len(merged), nil
}

// mergeDetailFields 把详情包合并进已有记录：只覆盖详情专属字段，其余沿用库里。
//
// 刻意不复用 mergePreserving：那个函数对城市/商圈/地点也做兜底，
// 而详情包的 CityName 可能来自请求参数兜底值，方向一旦反过来就会把
// 列表里正确的城市覆盖掉。这里只碰详情接口真正权威的六个字段。
func mergeDetailFields(stored, detail model.Job) model.Job {
	out := stored

	if v := strings.TrimSpace(detail.Description); v != "" {
		out.Description = v
	}
	if v := strings.TrimSpace(detail.ActiveTimeDesc); v != "" {
		out.ActiveTimeDesc = v
	}
	if v := strings.TrimSpace(detail.BossName); v != "" {
		out.BossName = v
	}
	if v := strings.TrimSpace(detail.BossTitle); v != "" {
		out.BossTitle = v
	}
	if v := strings.TrimSpace(detail.PublishedAt); v != "" {
		out.PublishedAt = v
		if src := strings.TrimSpace(detail.PublishedAtSource); src != "" {
			out.PublishedAtSource = src
		}
	}

	return out
}
