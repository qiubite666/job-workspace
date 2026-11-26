// Package dedupe 负责跨平台采集结果的去重。
//
// 两层规则：先按唯一 ID 合并同一岗位的重复抓取，再在同平台内按
// `公司|岗位|城市|行政区` 指纹合并重复曝光。冲突时保留信息更完整的一条，
// 输出顺序与首次出现顺序一致，保证多次采集结果稳定可比。
package dedupe

import (
	"strings"
	"time"

	"jobworkspace/internal/mapping"
	"jobworkspace/internal/model"
)

// Jobs 对岗位列表去重。
//
// 返回去重后的列表、被移除数量、按 ID 移除数、按指纹移除数。
// 入参 slice 不会被修改；输入为空时返回空切片（非 nil）。
func Jobs(jobs []model.Job) (out []model.Job, removed int, byID, byFingerprint int) {
	if len(jobs) == 0 {
		return []model.Job{}, 0, 0, 0
	}

	// 第一层：ID 去重。ID 为空视为无法判定重复，原样保留。
	stage := make([]model.Job, 0, len(jobs))
	idPos := make(map[string]int, len(jobs))
	for _, j := range jobs {
		key := strings.TrimSpace(j.ID)
		if key == "" {
			stage = append(stage, j)
			continue
		}
		if pos, ok := idPos[key]; ok {
			byID++
			if better(j, stage[pos]) {
				// 替换内容但保留首次出现的位置，输出顺序因此保持稳定。
				stage[pos] = j
			}
			continue
		}
		idPos[key] = len(stage)
		stage = append(stage, j)
	}

	// 第二层：同平台内按指纹去重。指纹退化为空（四个字段全空）时跳过，
	// 否则会把一堆无名岗位错误合并成一条。
	out = make([]model.Job, 0, len(stage))
	fpPos := make(map[string]int, len(stage))
	for _, j := range stage {
		fp := mapping.Fingerprint(j)
		if degenerateFingerprint(fp) {
			out = append(out, j)
			continue
		}
		key := strings.TrimSpace(j.Platform) + "\x00" + fp
		if pos, ok := fpPos[key]; ok {
			byFingerprint++
			if better(j, out[pos]) {
				// 同一 key 意味着平台与指纹都相同，替换后 key 仍然有效。
				out[pos] = j
			}
			continue
		}
		fpPos[key] = len(out)
		out = append(out, j)
	}

	return out, byID + byFingerprint, byID, byFingerprint
}

// better 判断 a 是否优于 b（用于冲突时择优保留）。
//
// 优先级：完整度得分 → 薪资明确度 → CollectedAt 更新者。
// 完全打平时返回 false，即保留先出现的一条。
func better(a, b model.Job) bool {
	if ca, cb := mapping.Completeness(a), mapping.Completeness(b); ca != cb {
		return ca > cb
	}
	if sa, sb := salaryClarity(a), salaryClarity(b); sa != sb {
		return sa > sb
	}
	return collectedAfter(a.CollectedAt, b.CollectedAt)
}

// collectedAfter 判断 a 的采集时间是否晚于 b。
//
// CollectedAt 来自 pipeline 传入的 ctx，格式可能是 UTC（Z）也可能是带偏移的 RFC3339
// （如 +08:00）；后者直接比字符串会错序，所以优先按时间解析比较，
// 任一解析失败再退回字典序（同格式下字典序等价于时间序）。
func collectedAfter(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == b:
		return false
	case a == "":
		return false
	case b == "":
		return true
	}
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA == nil && errB == nil {
		return ta.After(tb)
	}
	return a > b
}

// salaryClarity 把薪资明确度量化：区间/单值 2，面议 1，未知 0。
func salaryClarity(j model.Job) int {
	switch strings.ToLower(strings.TrimSpace(j.SalaryKind)) {
	case "range", "single":
		return 2
	case "negotiable":
		return 1
	default:
		return 0
	}
}

// degenerateFingerprint 判断指纹是否退化为全空（如 "|||"）。
//
// 这类记录缺少公司/岗位/城市/区域，不具备可合并性，必须各自保留。
func degenerateFingerprint(fp string) bool {
	return strings.TrimSpace(strings.Trim(fp, "|")) == ""
}
