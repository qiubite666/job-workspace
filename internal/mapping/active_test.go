package mapping

import (
	"strconv"
	"testing"
	"time"

	"jobworkspace/internal/model"
)

// TestActiveDescFromStatus 覆盖「在线 / 时间戳 / 都没有」三种情况。
//
// BOSS 列表给的是 bossOnline（布尔）与时间戳，不是现成文案，
// 归一逻辑错了会导致整批岗位的活跃状态全为空。
func TestActiveDescFromStatus(t *testing.T) {
	boss := func(raw map[string]any) string {
		return activeDescFromStatus(raw, "bossOnline", "lastModifyTime")
	}

	if got := boss(map[string]any{"bossOnline": true}); got != "在线" {
		t.Errorf("在线判定错误: %q", got)
	}

	now := time.Now()
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"十分钟前", map[string]any{"lastModifyTime": float64(now.Add(-10 * time.Minute).UnixMilli())}, "刚刚活跃"},
		{"五小时前", map[string]any{"lastModifyTime": float64(now.Add(-5 * time.Hour).UnixMilli())}, "5小时前活跃"},
		{"三天前", map[string]any{"lastModifyTime": float64(now.Add(-3 * 24 * time.Hour).UnixMilli())}, "3天前活跃"},
		{"过久", map[string]any{"lastModifyTime": float64(now.Add(-100 * 24 * time.Hour).UnixMilli())}, ""},
		{"字符串时间戳", map[string]any{"lastModifyTime": strconv.FormatInt(now.Add(-2*time.Hour).UnixMilli(), 10)}, "2小时前活跃"},
		{"缺字段", map[string]any{}, ""},
		{"在线优先于时间", map[string]any{"bossOnline": true, "lastModifyTime": float64(now.UnixMilli())}, "在线"},
	}
	for _, c := range cases {
		got := boss(c.raw)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestBuildDraftFillsActiveDesc 确认映射链路真的把这套逻辑接上了。
func TestBuildDraftFillsActiveDesc(t *testing.T) {
	spec := Spec{
		Platform:         "boss",
		PlatformName:     "BOSS直聘",
		FieldMap:         map[string][]string{"jobName": {"jobName"}},
		ActiveOnlinePath: "bossOnline",
		ActiveTimePath:   "lastModifyTime",
	}
	d := BuildDraft(map[string]any{"jobName": "运维", "bossOnline": true}, spec)
	if d.ActiveTimeDesc != "在线" {
		t.Errorf("BuildDraft 未填充活跃状态: %q", d.ActiveTimeDesc)
	}

	// 平台没配路径时不应凭空造值。
	empty := BuildDraft(map[string]any{"jobName": "运维", "bossOnline": true},
		Spec{Platform: "boss", FieldMap: map[string][]string{}})
	if empty.ActiveTimeDesc != "" {
		t.Errorf("未配置路径却产生了活跃状态: %q", empty.ActiveTimeDesc)
	}
	_ = model.Job{}
}
