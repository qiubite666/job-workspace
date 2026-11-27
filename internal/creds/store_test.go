package creds

import (
	"os"
	"testing"
)

// TestTokensArePerUser 每个账号的令牌必须不同、可反查、互不通用。
func TestTokensArePerUser(t *testing.T) {
	st := New(t.TempDir())

	ta, err := st.Ensure("alice")
	if err != nil {
		t.Fatalf("alice 生成令牌失败: %v", err)
	}
	tb, err := st.Ensure("bob")
	if err != nil {
		t.Fatalf("bob 生成令牌失败: %v", err)
	}
	if ta == tb {
		t.Fatal("两个账号拿到了同一个令牌")
	}
	if len(ta) != 48 {
		t.Errorf("令牌长度 = %d，期望 48", len(ta))
	}

	if u, ok := st.Resolve(ta); !ok || u != "alice" {
		t.Errorf("alice 令牌反查失败: %q %v", u, ok)
	}
	if u, ok := st.Resolve(tb); !ok || u != "bob" {
		t.Errorf("bob 令牌反查失败: %q %v", u, ok)
	}
	if _, ok := st.Resolve("deadbeef"); ok {
		t.Error("无效令牌竟然反查成功")
	}
}

// TestEnsureIsStable 重复调用 Ensure 不应换令牌。
func TestEnsureIsStable(t *testing.T) {
	st := New(t.TempDir())
	first, _ := st.Ensure("alice")
	second, _ := st.Ensure("alice")
	if first != second {
		t.Fatal("Ensure 每次调用都换了令牌")
	}
}

// TestRotateInvalidatesOld 重置后旧令牌立即失效。
func TestRotateInvalidatesOld(t *testing.T) {
	st := New(t.TempDir())
	old, _ := st.Ensure("alice")

	newToken, err := st.Rotate("alice")
	if err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if newToken == old {
		t.Fatal("重置后令牌没变")
	}
	if _, ok := st.Resolve(old); ok {
		t.Error("旧令牌仍然可用")
	}
	if u, ok := st.Resolve(newToken); !ok || u != "alice" {
		t.Error("新令牌不可用")
	}
}

// TestCookiePerUser cookie 是账号私有，不能串。
func TestCookiePerUser(t *testing.T) {
	st := New(t.TempDir())
	if _, err := st.Ensure("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ensure("bob"); err != nil {
		t.Fatal(err)
	}

	if err := st.SetCookie("alice", "a-cookie"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	ca, _ := st.Get("alice")
	cb, _ := st.Get("bob")
	if ca.Cookie != "a-cookie" {
		t.Errorf("alice 的 cookie 未保存: %q", ca.Cookie)
	}
	if cb.Cookie != "" {
		t.Errorf("bob 读到了别人的 cookie: %q", cb.Cookie)
	}

	if err := st.ClearCookie("alice"); err != nil {
		t.Fatal(err)
	}
	ca, _ = st.Get("alice")
	if ca.Cookie != "" {
		t.Errorf("清除后仍有 cookie: %q", ca.Cookie)
	}
}

// TestCookieWithoutTokenCreatesOne 只填 cookie 时也要有令牌。
func TestCookieWithoutTokenCreatesOne(t *testing.T) {
	st := New(t.TempDir())
	if err := st.SetCookie("alice", "x"); err != nil {
		t.Fatal(err)
	}
	c, _ := st.Get("alice")
	if c.Token == "" {
		t.Error("只填 cookie 时没有生成令牌")
	}
}

// TestCookieTooLong cookie 有体积上限。
func TestCookieTooLong(t *testing.T) {
	st := New(t.TempDir())
	big := make([]byte, maxCookieLen+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := st.SetCookie("alice", string(big)); err == nil {
		t.Error("超长 cookie 未被拒绝")
	}
}

// TestBadUsernameRejected 挡住路径穿越。
func TestBadUsernameRejected(t *testing.T) {
	st := New(t.TempDir())
	for _, name := range []string{"../evil", "a/b", "", "ab", ".."} {
		if _, err := st.Ensure(name); err == nil {
			t.Errorf("非法账号名 %q 未被拒绝", name)
		}
	}
}

// TestAdoptLegacyToken 旧全局令牌迁移一次，不覆盖已有。
func TestAdoptLegacyToken(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)

	legacy := dir + "/ingest.token"
	if err := os.WriteFile(legacy, []byte("legacy-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, err := st.AdoptLegacyToken(legacy, "alice")
	if err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	if !ok {
		t.Fatal("未接管旧令牌")
	}
	if u, found := st.Resolve("legacy-token"); !found || u != "alice" {
		t.Error("旧令牌未能反查到账号")
	}

	// 二次调用不应覆盖
	ok2, _ := st.AdoptLegacyToken(legacy, "bob")
	if ok2 {
		t.Error("旧令牌被重复接管")
	}
}
