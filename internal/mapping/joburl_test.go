package mapping

import "testing"

// TestWithSecurityID 详情链接必须带上 securityId，否则 BOSS 打开是空壳页，
// 页面不会请求详情接口，扩展也就抓不到 JD。
func TestWithSecurityID(t *testing.T) {
	cases := []struct {
		name string
		url  string
		sid  string
		want string
	}{
		{"补参数", "https://www.zhipin.com/job_detail/a.html", "sid123",
			"https://www.zhipin.com/job_detail/a.html?securityId=sid123"},
		{"已有查询串用 &", "https://x/a.html?from=list", "sid123",
			"https://x/a.html?from=list&securityId=sid123"},
		{"已带则不动", "https://x/a.html?securityId=old", "sid123",
			"https://x/a.html?securityId=old"},
		{"无 securityId 保持原样", "https://x/a.html", "", "https://x/a.html"},
		{"无链接保持空", "", "sid123", ""},
	}
	for _, c := range cases {
		if got := withSecurityID(c.url, c.sid); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestBuildDraftCarriesSecurityID 列表包里的 securityId 要能落到模型上。
func TestBuildDraftCarriesSecurityID(t *testing.T) {
	spec := Spec{
		Platform:       "boss",
		PlatformName:   "BOSS直聘",
		FieldMap:       map[string][]string{"securityId": {"securityId"}, "jobName": {"jobName"}, "externalId": {"encryptJobId"}},
		JobURLTemplate: "https://www.zhipin.com/job_detail/{{externalId}}.html",
	}
	d := BuildDraft(map[string]any{
		"encryptJobId": "abc",
		"securityId":   "sid-1",
		"jobName":      "SRE",
	}, spec)

	if d.SecurityID != "sid-1" {
		t.Errorf("securityId 未映射: %q", d.SecurityID)
	}

	j := Finalize(d, spec, Context{})
	if j.JobURL != "https://www.zhipin.com/job_detail/abc.html?securityId=sid-1" {
		t.Errorf("链接未带 securityId: %q", j.JobURL)
	}
}
