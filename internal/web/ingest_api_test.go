package web

import (
	"testing"

	"jobworkspace/internal/config"
)

// TestSpecFromPlatformCarriesActivePaths 守住「配置透传」这一环。
//
// 新增配置项时最容易漏的就是中间层：activeOnlinePath 当初只接到了 CLI 采集，
// ingest 自己拼 Spec 时漏了，结果扩展上报的 633 条数据活跃状态全为空，
// 而 CLI 采集路径却是好的——单测覆盖 CLI 完全发现不了。
func TestSpecFromPlatformCarriesActivePaths(t *testing.T) {
	p := &config.Platform{
		ID:               "boss",
		Name:             "BOSS直聘",
		JobURLTemplate:   "https://example.com/{{externalId}}.html",
		PublishTimePaths: []string{"postTime"},
		ActiveOnlinePath: "bossOnline",
		ActiveTimePath:   "lastModifyTime",
		FieldMap:         map[string][]string{"jobName": {"jobName"}},
	}

	spec := specFromPlatform(p)

	if spec.ActiveOnlinePath != "bossOnline" {
		t.Errorf("activeOnlinePath 未透传: %q", spec.ActiveOnlinePath)
	}
	if spec.ActiveTimePath != "lastModifyTime" {
		t.Errorf("activeTimePath 未透传: %q", spec.ActiveTimePath)
	}
	if spec.Platform != "boss" || spec.PlatformName != "BOSS直聘" {
		t.Errorf("平台标识透传错误: %q/%q", spec.Platform, spec.PlatformName)
	}
	if spec.JobURLTemplate == "" || len(spec.PublishTimePaths) != 1 || len(spec.FieldMap) != 1 {
		t.Error("其余字段透传不完整")
	}
}
