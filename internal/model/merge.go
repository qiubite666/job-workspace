package model

import "strings"

// MergePreserving 用 incoming 覆盖 existing，但 incoming 的空值不抹掉已有内容。
//
// 列表接口不返回职位描述，如果直接整条替换，用户点开详情补全的 JD 会在下一次
// 列表上报时被清空。这里按字段做「有则覆盖、无则保留」的合并。
func MergePreserving(existing, incoming Job) Job {
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

// MergeDetailFields 把详情包合并进已有记录：只覆盖详情专属字段，其余沿用库里。
//
// 刻意不复用 MergePreserving：那个函数对城市/商圈/地点也做兜底，
// 而详情包的 CityName 可能来自请求参数兜底值，方向一旦反过来就会把
// 列表里正确的城市覆盖掉。这里只碰详情接口真正权威的六个字段。
func MergeDetailFields(stored, detail Job) Job {
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
