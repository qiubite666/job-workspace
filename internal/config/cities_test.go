package config

import (
	"os"
	"testing"
)

// TestBuiltinCitiesMerged 确认内置城市码表已并入 BOSS 平台。
//
// 城市表是「采集任意城市」的前提：resolveCity 只认表里的名字，
// 表里没有就会把中文城市名原样当城市码发出去，必然查不到数据。
func TestBuiltinCitiesMerged(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载内置配置失败: %v", err)
	}
	boss := cfg.Platform("boss")
	if boss == nil {
		t.Fatal("未找到 boss 平台")
	}
	if len(boss.Cities) < 300 {
		t.Fatalf("城市表过小: %d 个，期望 300+", len(boss.Cities))
	}
	cases := map[string]string{
		"呼和浩特": "101080100",
		"赣州":   "101240700",
		"北京":   "101010100",
	}
	for name, want := range cases {
		if got := boss.Cities[name]; got != want {
			t.Errorf("城市 %s 的城市码 = %q，期望 %q", name, got, want)
		}
	}
}

// TestExternalCitiesOverrideBuiltin 确认外部配置可以覆盖单个城市码。
func TestExternalCitiesOverrideBuiltin(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/platforms.json"
	body := `{
	  "version": 1,
	  "defaults": {"pages": 1, "pageSize": 30},
	  "platforms": {
	    "boss": {
	      "id": "boss",
	      "name": "BOSS直聘",
	      "searchUrl": "https://example.com",
	      "cities": {"北京": "999999999"},
	      "fieldMap": {}
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	boss := cfg.Platform("boss")
	if got := boss.Cities["北京"]; got != "999999999" {
		t.Errorf("外部配置未覆盖内置城市码: %q", got)
	}
	// 未被覆盖的城市仍应来自内置表。
	if got := boss.Cities["呼和浩特"]; got != "101080100" {
		t.Errorf("内置城市被误删: %q", got)
	}
}
