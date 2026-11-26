package store

import (
	"jobworkspace/internal/model"
)

// GetJob 按 ID 查找单个岗位。
//
// 数据来源与 LoadJobs 一致：只读 jobs.json，不存在时视为无数据。
// 未找到时返回 (零值, false, nil)，不把「不存在」当成错误。
//
// 注意：这里持的是与 MergeAndSave / SaveJobs 同一把 s.mu，
// 因此和采集落盘不会产生竞态。
func (s *Store) GetJob(id string) (model.Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	jobs, _, err := s.readJobsForMutationLocked()
	if err != nil {
		return model.Job{}, false, err
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, true, nil
		}
	}
	return model.Job{}, false, nil
}

// UpsertJob 按 ID 覆盖或追加一条岗位。
//
// 已存在时整体替换该位置的记录，其余岗位的相对顺序与内容保持不变；
// 不存在时追加到末尾。created 表示本次是否为新增。
func (s *Store) UpsertJob(j model.Job) (created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	jobs, _, err := s.readJobsForMutationLocked()
	if err != nil {
		return false, err
	}

	replaced := false
	for i := range jobs {
		if jobs[i].ID == j.ID {
			jobs[i] = j
			replaced = true
			break
		}
	}
	if !replaced {
		jobs = append(jobs, j)
	}

	if err := s.saveJobsLocked(jobs); err != nil {
		return false, err
	}
	return !replaced, nil
}

// DeleteJob 按 ID 删除一条岗位，其余岗位顺序不变。
//
// 未找到时返回 (false, nil)，由调用方决定是否回 404。
func (s *Store) DeleteJob(id string) (existed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	jobs, _, err := s.readJobsForMutationLocked()
	if err != nil {
		return false, err
	}

	out := make([]model.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.ID == id {
			existed = true
			continue
		}
		out = append(out, j)
	}
	if !existed {
		return false, nil
	}
	if err := s.saveJobsLocked(out); err != nil {
		return false, err
	}
	return true, nil
}

// ListJobs 返回「可管理」的岗位集合：只读 jobs.json，不回退示例数据。
//
// 与 LoadJobs 的差别：LoadJobs 会顺带返回数据来源（collected / empty），
// 后台管理只需要数据本身，且同样绝不允许把不存在的数据当成可编辑基线，
// 否则会出现「列表里有、点编辑却 404」，以及「空态被写进采集产物、
// source 从 empty 变成 collected」这类失真。
func (s *Store) ListJobs() ([]model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, ok, err := readJobsFile(s.jobsPath())
	if err != nil {
		return nil, err
	}
	if !ok {
		return []model.Job{}, nil
	}
	return jobs, nil
}

// readJobsForMutationLocked 读取「可编辑」的岗位集合，调用方必须已持有 s.mu。
//
// 只以 jobs.json 为基线：不存在时视为空集合，绝不把示例数据当作可写基线。
func (s *Store) readJobsForMutationLocked() ([]model.Job, string, error) {
	jobs, ok, err := readJobsFile(s.jobsPath())
	if err != nil {
		return nil, SourceEmpty, err
	}
	if ok {
		return jobs, SourceCollected, nil
	}
	return []model.Job{}, SourceEmpty, nil
}
