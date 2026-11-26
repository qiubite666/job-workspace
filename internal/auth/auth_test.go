package auth

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testIterations 是测试用迭代数，只为跑得快；生产值由 HashPassword 固定为 210000。
const testIterations = 1_000

// ---------- PBKDF2 ----------

// TestPBKDF2KeyVectors 用 PBKDF2-HMAC-SHA256 公认测试向量逐字节校验实现。
func TestPBKDF2KeyVectors(t *testing.T) {
	cases := []struct {
		iterations int
		wantHex    string
	}{
		{1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(PBKDF2Key([]byte("password"), []byte("salt"), c.iterations, 32))
		t.Logf("P=%q S=%q c=%d dkLen=32 → %s", "password", "salt", c.iterations, got)
		if got != c.wantHex {
			t.Errorf("PBKDF2Key(iter=%d) = %s, 期望 %s", c.iterations, got, c.wantHex)
		}
	}
}

// TestPBKDF2KeyLongerOutput 验证多块派生：前 32 字节与单块结果一致，长度恰好为请求值。
func TestPBKDF2KeyLongerOutput(t *testing.T) {
	short := PBKDF2Key([]byte("password"), []byte("salt"), 4096, 32)
	long := PBKDF2Key([]byte("password"), []byte("salt"), 4096, 40)
	if len(long) != 40 {
		t.Fatalf("期望 40 字节，实际 %d", len(long))
	}
	if hex.EncodeToString(long[:32]) != hex.EncodeToString(short) {
		t.Error("多块派生的第一块应与单块派生完全一致")
	}
	// 第二块的前 8 字节也应与「单独派生 32 字节」无关，只做非零校验防止误返回。
	for _, b := range long[32:] {
		if b != 0 {
			return
		}
	}
	t.Error("第二块不应全为零")
}

// TestPBKDF2KeyEdgeCases 验证非法参数不会 panic。
func TestPBKDF2KeyEdgeCases(t *testing.T) {
	if got := PBKDF2Key([]byte("p"), []byte("s"), 0, 0); got != nil {
		t.Errorf("keyLen=0 期望 nil，实际 %v", got)
	}
	one := PBKDF2Key([]byte("p"), []byte("s"), 1, 16)
	if len(one) != 16 {
		t.Errorf("期望 16 字节，实际 %d", len(one))
	}
	// iter < 1 视为 1。
	if hex.EncodeToString(PBKDF2Key([]byte("p"), []byte("s"), -5, 16)) != hex.EncodeToString(one) {
		t.Error("iter<1 应按 1 轮处理")
	}
}

// ---------- 口令哈希 ----------

func TestHashPasswordRoundTrip(t *testing.T) {
	encoded, err := HashPasswordWith("correct horse battery", 2_000, 16, 32)
	if err != nil {
		t.Fatalf("HashPasswordWith 失败: %v", err)
	}

	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		t.Fatalf("编码应有 4 段，实际 %d 段: %s", len(parts), encoded)
	}
	if parts[0] != "pbkdf2-sha256" || parts[1] != "2000" {
		t.Errorf("算法与迭代数不符: %s$%s", parts[0], parts[1])
	}
	if !VerifyPassword(encoded, "correct horse battery") {
		t.Error("正确密码应校验通过")
	}
	if VerifyPassword(encoded, "correct horse batter") {
		t.Error("错误密码不应校验通过")
	}
	if VerifyPassword(encoded, "") {
		t.Error("空密码不应校验通过")
	}
}

// TestHashPasswordSaltUniqueness 同一口令两次哈希必须不同（随机盐）。
func TestHashPasswordSaltUniqueness(t *testing.T) {
	a, err := HashPasswordWith("same-password", testIterations, 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPasswordWith("same-password", testIterations, 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("两次哈希结果不应相同")
	}
	if !VerifyPassword(a, "same-password") || !VerifyPassword(b, "same-password") {
		t.Error("两个哈希都应校验通过")
	}
}

// TestHashPasswordDefault 确认生产参数：210000 轮、三段式编码可被 VerifyPassword 接受。
func TestHashPasswordDefault(t *testing.T) {
	encoded, err := HashPassword("a-strong-password-2026")
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$210000$") {
		t.Errorf("生产参数应为 210000 轮，实际 %s", encoded)
	}
	if !VerifyPassword(encoded, "a-strong-password-2026") {
		t.Error("生产参数生成的哈希应校验通过")
	}
}

func TestHashPasswordWithInvalidParams(t *testing.T) {
	cases := []struct {
		name                    string
		iter, saltLen, keyLenIn int
	}{
		{"iter=0", 0, 16, 32},
		{"iter 过大", maxIterations + 1, 16, 32},
		{"salt 太短", 1000, 4, 32},
		{"salt 过长", 1000, 128, 32},
		{"key 太短", 1000, 16, 8},
		{"key 过长", 1000, 16, 128},
	}
	for _, c := range cases {
		if _, err := HashPasswordWith("password123", c.iter, c.saltLen, c.keyLenIn); err == nil {
			t.Errorf("%s: 期望返回错误", c.name)
		}
	}
}

// TestVerifyPasswordRejects 覆盖「错误密码 / 篡改哈希 / 非法格式」三条路径。
func TestVerifyPasswordRejects(t *testing.T) {
	encoded, err := HashPasswordWith("original-password", testIterations, 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(encoded, "$")

	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	hashBytes, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatal(err)
	}

	// 篡改哈希：翻转一个字节后重新编码。
	tamperedHash := append([]byte(nil), hashBytes...)
	tamperedHash[0] ^= 0xFF
	// 篡改盐：翻转一个字节。
	tamperedSalt := append([]byte(nil), salt...)
	tamperedSalt[0] ^= 0xFF

	b64 := base64.StdEncoding.EncodeToString
	cases := []struct {
		name     string
		encoded  string
		password string
	}{
		{"错误密码", encoded, "wrong-password"},
		{"篡改哈希", "pbkdf2-sha256$1000$" + b64(salt) + "$" + b64(tamperedHash), "original-password"},
		{"篡改盐", "pbkdf2-sha256$1000$" + b64(tamperedSalt) + "$" + b64(hashBytes), "original-password"},
		{"空串", "", "original-password"},
		{"段数不足", "pbkdf2-sha256$1000$" + b64(salt), "original-password"},
		{"段数过多", encoded + "$extra", "original-password"},
		{"算法未知", "bcrypt$1000$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"缺失算法前缀", "$1000$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"迭代数为空", "pbkdf2-sha256$$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"迭代数为零", "pbkdf2-sha256$0$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"迭代数为负", "pbkdf2-sha256$-1$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"迭代数越界", "pbkdf2-sha256$1000001$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"迭代数非数字", "pbkdf2-sha256$abc$" + b64(salt) + "$" + b64(hashBytes), "original-password"},
		{"盐非法 base64", "pbkdf2-sha256$1000$!!!!$" + b64(hashBytes), "original-password"},
		{"盐为空", "pbkdf2-sha256$1000$$" + b64(hashBytes), "original-password"},
		{"哈希非法 base64", "pbkdf2-sha256$1000$" + b64(salt) + "$!!!!", "original-password"},
		{"哈希为空", "pbkdf2-sha256$1000$" + b64(salt) + "$", "original-password"},
	}
	for _, c := range cases {
		if VerifyPassword(c.encoded, c.password) {
			t.Errorf("%s: 应返回 false", c.name)
		}
	}
}

// ---------- 用户存储 ----------

// newTestStore 在临时目录里建一个已加载的空仓库。
func newTestStore(t *testing.T) *UserStore {
	t.Helper()
	s := NewUserStore(filepath.Join(t.TempDir(), "users.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("Load 空仓库失败: %v", err)
	}
	return s
}

// seedAdmin 建一个管理员并返回其公开视图。
func seedAdmin(t *testing.T, s *UserStore) PublicUser {
	t.Helper()
	u, err := s.Create("admin", "管理员", RoleAdmin, "admin-password-1")
	if err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	return u
}

func TestCreateAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "users.json")
	s := NewUserStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	u, err := s.Create("  Alice.Admin  ", "", RoleAdmin, "alice-password-1")
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if u.Username != "alice.admin" {
		t.Errorf("用户名应 trim+小写，实际 %q", u.Username)
	}
	if u.DisplayName != "alice.admin" {
		t.Errorf("空显示名应回落为用户名，实际 %q", u.DisplayName)
	}
	if u.CreatedAt == "" || u.CreatedAt != u.UpdatedAt {
		t.Errorf("创建时间应写入且与更新时间一致: %+v", u)
	}
	if s.Count() != 1 {
		t.Errorf("Count 期望 1，实际 %d", s.Count())
	}

	// 重新加载：内容与权限都应正确。
	s2 := NewUserStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("二次 Load 失败: %v", err)
	}
	got, ok := s2.Get("alice.admin")
	if !ok {
		t.Fatal("重新加载后应能取到用户")
	}
	if got.Role != RoleAdmin || !VerifyPassword(got.PasswordHash, "alice-password-1") {
		t.Error("重新加载后角色或密码哈希不正确")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("用户文件权限期望 0600，实际 %04o", perm)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("目录权限期望 0700，实际 %04o", perm)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f userFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘 JSON 非法: %v", err)
	}
	if f.Version != 1 {
		t.Errorf("version 期望 1，实际 %d", f.Version)
	}
	if len(f.Users) != 1 || f.Users[0].PasswordHash == "" {
		t.Errorf("落盘内容应含 1 个带哈希的用户: %+v", f.Users)
	}
}

// TestStoreAutoLoads 防止忘记 Load 时把已有用户覆盖掉。
func TestStoreAutoLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	s1 := NewUserStore(path)
	if _, err := s1.Create("admin", "", RoleAdmin, "admin-password-1"); err != nil {
		t.Fatal(err)
	}

	s2 := NewUserStore(path) // 故意不调用 Load
	if _, err := s2.Create("viewer", "", RoleViewer, "viewer-password-1"); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if s2.Count() != 2 {
		t.Errorf("自动加载后 Count 期望 2，实际 %d", s2.Count())
	}

	s3 := NewUserStore(path)
	if err := s3.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s3.Get("admin"); !ok {
		t.Error("首次写入的用户不应被覆盖")
	}
}

func TestCreateValidation(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)

	cases := []struct {
		name     string
		username string
		role     Role
		password string
		wantErr  error
	}{
		{"重复用户", "admin", RoleViewer, "another-password", ErrUserExists},
		{"重复用户（大小写/空白不同）", "  ADMIN  ", RoleViewer, "another-password", ErrUserExists},
		{"密码太短", "shorty", RoleViewer, "1234567", ErrWeakPassword},
		{"密码太长", "longy", RoleViewer, strings.Repeat("a", 129), ErrWeakPassword},
		{"密码全空白", "blanky", RoleViewer, "        ", ErrWeakPassword},
		{"角色非法", "badrole", Role("owner"), "password-123", ErrInvalidRole},
		{"角色为空", "emptyrole", "", "password-123", ErrInvalidRole},
	}
	for _, c := range cases {
		_, err := s.Create(c.username, "", c.role, c.password)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.wantErr, err)
		}
	}

	invalidNames := []string{"ab", strings.Repeat("a", 33), "has space", "bad!char", "中文用户", ""}
	for _, name := range invalidNames {
		if _, err := s.Create(name, "", RoleViewer, "password-123"); err == nil {
			t.Errorf("非法用户名 %q 应报错", name)
		}
	}

	if s.Count() != 1 {
		t.Errorf("失败的创建不应写入，Count 期望 1，实际 %d", s.Count())
	}

	// 边界：8 位与 128 位密码都应通过。
	if _, err := s.Create("min.pw", "", RoleViewer, "12345678"); err != nil {
		t.Errorf("8 位密码应通过: %v", err)
	}
	if _, err := s.Create("max.pw", "", RoleViewer, strings.Repeat("密", 128)); err != nil {
		t.Errorf("128 位（字符）密码应通过: %v", err)
	}
}

func TestParseRole(t *testing.T) {
	for _, in := range []string{"admin", "ADMIN", " Admin "} {
		if r, err := ParseRole(in); err != nil || r != RoleAdmin {
			t.Errorf("ParseRole(%q) = %v, %v", in, r, err)
		}
	}
	if r, err := ParseRole("viewer"); err != nil || r != RoleViewer {
		t.Errorf("ParseRole(viewer) = %v, %v", r, err)
	}
	for _, in := range []string{"", "root", "Admin2", "관리자"} {
		if _, err := ParseRole(in); !errors.Is(err, ErrInvalidRole) {
			t.Errorf("ParseRole(%q) 应返回 ErrInvalidRole，实际 %v", in, err)
		}
	}
}

func TestListSortedAndNoHashLeak(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"zed", "admin", "bob"} {
		role := RoleViewer
		if name == "admin" {
			role = RoleAdmin
		}
		if _, err := s.Create(name, "", role, "password-123"); err != nil {
			t.Fatal(err)
		}
	}

	list := s.List()
	if len(list) != 3 {
		t.Fatalf("List 期望 3 条，实际 %d", len(list))
	}
	if list[0].Username != "admin" || list[1].Username != "bob" || list[2].Username != "zed" {
		t.Errorf("应按 username 升序，实际 %v", []string{list[0].Username, list[1].Username, list[2].Username})
	}

	raw, err := json.Marshal(list[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "passwordHash") || strings.Contains(strings.ToLower(string(raw)), "pbkdf2") {
		t.Errorf("公开视图不得包含密码字段: %s", raw)
	}
}

func TestVerify(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)
	if _, err := s.Create("reader", "", RoleViewer, "reader-password-1"); err != nil {
		t.Fatal(err)
	}

	u, err := s.Verify(" admin ", "admin-password-1")
	if err != nil {
		t.Fatalf("正确凭据应通过: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("角色期望 admin，实际 %s", u.Role)
	}

	if _, err := s.Verify("admin", "wrong-password"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("错误密码期望 ErrInvalidPassword，实际 %v", err)
	}
	if _, err := s.Verify("ghost", "admin-password-1"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("用户不存在也应返回 ErrInvalidPassword，实际 %v", err)
	}

	// 禁用后登录返回 ErrUserDisabled。
	disabled := true
	if _, err := s.Update("reader", nil, nil, &disabled); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	if _, err := s.Verify("reader", "reader-password-1"); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("禁用账号期望 ErrUserDisabled，实际 %v", err)
	}
}

func TestSetPassword(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)

	if err := s.SetPassword("admin", "brand-new-password"); err != nil {
		t.Fatalf("SetPassword 失败: %v", err)
	}
	if _, err := s.Verify("admin", "admin-password-1"); !errors.Is(err, ErrInvalidPassword) {
		t.Error("旧密码应失效")
	}
	if _, err := s.Verify("admin", "brand-new-password"); err != nil {
		t.Errorf("新密码应通过: %v", err)
	}
	if err := s.SetPassword("admin", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("弱密码期望 ErrWeakPassword，实际 %v", err)
	}
	if err := s.SetPassword("ghost", "brand-new-password"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("用户不存在期望 ErrUserNotFound，实际 %v", err)
	}
}

func TestUpdatePartial(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)

	name := "新的显示名"
	u, err := s.Update("admin", &name, nil, nil)
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	if u.DisplayName != name || u.Role != RoleAdmin {
		t.Errorf("局部更新不正确: %+v", u)
	}

	if _, err := s.Update("ghost", &name, nil, nil); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("用户不存在期望 ErrUserNotFound，实际 %v", err)
	}

	if _, err := s.Update("admin", nil, nil, nil); err != nil {
		t.Errorf("全 nil 更新应成功: %v", err)
	}

	if err := s.Delete("ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("删除不存在用户期望 ErrUserNotFound，实际 %v", err)
	}
}

// TestLastAdminProtection 覆盖删除、降级、禁用三条路径。
func TestLastAdminProtection(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)

	if err := s.Delete("admin"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("删除最后一个管理员期望 ErrLastAdmin，实际 %v", err)
	}
	if s.Count() != 1 {
		t.Errorf("失败的删除不应生效，Count 期望 1，实际 %d", s.Count())
	}

	viewer := RoleViewer
	if _, err := s.Update("admin", nil, &viewer, nil); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("降级最后一个管理员期望 ErrLastAdmin，实际 %v", err)
	}

	disabled := true
	if _, err := s.Update("admin", nil, nil, &disabled); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("禁用最后一个管理员期望 ErrLastAdmin，实际 %v", err)
	}

	if u, ok := s.Get("admin"); !ok || u.Role != RoleAdmin || u.Disabled {
		t.Error("被拒绝的更新不应修改任何字段")
	}
}

// TestLastAdminAllowsWhenOthersExist 有第二个管理员时都能操作。
func TestLastAdminAllowsWhenOthersExist(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)
	if _, err := s.Create("admin2", "", RoleAdmin, "admin2-password"); err != nil {
		t.Fatal(err)
	}

	viewer := RoleViewer
	if _, err := s.Update("admin2", nil, &viewer, nil); err != nil {
		t.Errorf("还有其他管理员时降级应成功: %v", err)
	}
	if err := s.Delete("admin2"); err != nil {
		t.Errorf("还有其他管理员时删除应成功: %v", err)
	}
	if s.Count() != 1 {
		t.Errorf("Count 期望 1，实际 %d", s.Count())
	}
}

// TestDisabledAdminNotCounted 被禁用的管理员无法登录，不能充当「最后一个管理员」的挡箭牌。
func TestDisabledAdminNotCounted(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)
	if _, err := s.Create("admin2", "", RoleAdmin, "admin2-password"); err != nil {
		t.Fatal(err)
	}

	disabled := true
	if _, err := s.Update("admin2", nil, nil, &disabled); err != nil {
		t.Fatalf("禁用第二个管理员应成功: %v", err)
	}
	// 此时唯一可用管理员是 admin，不能再删。
	if err := s.Delete("admin"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("期望 ErrLastAdmin，实际 %v", err)
	}
	// 删除被禁用的那个是允许的。
	if err := s.Delete("admin2"); err != nil {
		t.Errorf("删除已禁用账号应成功: %v", err)
	}
}

func TestLoadCases(t *testing.T) {
	dir := t.TempDir()

	// 文件不存在 → 空集合，不报错。
	missing := NewUserStore(filepath.Join(dir, "missing.json"))
	if err := missing.Load(); err != nil {
		t.Errorf("文件不存在不应报错: %v", err)
	}
	if missing.Count() != 0 {
		t.Errorf("期望空集合，实际 %d", missing.Count())
	}

	// 内容损坏 → 带上下文 error，且不清空已有内存数据。
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := NewUserStore(badPath)
	err := bad.Load()
	if err == nil {
		t.Fatal("损坏文件应报错")
	}
	if !strings.Contains(err.Error(), badPath) {
		t.Errorf("错误应包含文件路径，实际 %v", err)
	}

	// 空文件 → 报错（不静默当成空集合）。
	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewUserStore(emptyPath).Load(); err == nil {
		t.Error("空文件应报错")
	}

	// 版本不支持 → 报错。
	verPath := filepath.Join(dir, "v2.json")
	if err := os.WriteFile(verPath, []byte(`{"version":2,"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewUserStore(verPath).Load(); err == nil {
		t.Error("不支持的版本应报错")
	}

	// 合法但缺 version 字段 → 按 v1 接受。
	legacyPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewUserStore(legacyPath).Load(); err != nil {
		t.Errorf("缺 version 应兼容: %v", err)
	}
}

// TestFailedLoadKeepsMemory Load 失败时不得清空内存中的用户。
func TestFailedLoadKeepsMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	s := NewUserStore(path)
	if _, err := s.Create("admin", "", RoleAdmin, "admin-password-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err == nil {
		t.Fatal("期望 Load 报错")
	}
	if s.Count() != 1 {
		t.Errorf("Load 失败后内存应保持不变，Count 期望 1，实际 %d", s.Count())
	}
}

// TestConcurrentCreate 在并发下不会丢用户（配合 -race 使用更佳）。
func TestConcurrentCreate(t *testing.T) {
	s := newTestStore(t)
	seedAdmin(t, s)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Create("user"+string(rune('a'+i)), "", RoleViewer, "password-123")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("并发创建第 %d 个失败: %v", i, err)
		}
	}
	if s.Count() != n+1 {
		t.Errorf("Count 期望 %d，实际 %d", n+1, s.Count())
	}
}

// ---------- 会话 ----------

func TestSessionCreateAndGet(t *testing.T) {
	m := NewSessionManager(time.Hour)
	u := PublicUser{Username: "admin", Role: RoleAdmin}

	a, err := m.Create(u)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	b, err := m.Create(u)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if a.ID == b.ID {
		t.Error("会话 ID 必须唯一")
	}
	if len(a.ID) != 43 { // 32 字节 base64url 无填充 = 43 字符
		t.Errorf("会话 ID 长度期望 43，实际 %d", len(a.ID))
	}
	if a.Username != "admin" || a.Role != RoleAdmin {
		t.Errorf("会话应携带用户信息: %+v", a)
	}
	if got := a.ExpiresAt.Sub(a.CreatedAt); got != time.Hour {
		t.Errorf("有效期期望 1h，实际 %v", got)
	}

	got, ok := m.Get(a.ID)
	if !ok || got.Username != "admin" {
		t.Error("应能取回会话")
	}
	if _, ok := m.Get("not-exists"); ok {
		t.Error("不存在的会话应返回 false")
	}
	if _, ok := m.Get(""); ok {
		t.Error("空 ID 应返回 false")
	}
	if m.Len() != 2 {
		t.Errorf("Len 期望 2，实际 %d", m.Len())
	}
}

// TestSessionExpiryAndRenewal 用注入时钟精确控制过期与滑动续期。
func TestSessionExpiryAndRenewal(t *testing.T) {
	ttl := time.Hour
	m := NewSessionManager(ttl)
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	sess, err := m.Create(PublicUser{Username: "admin", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	originalExpiry := sess.ExpiresAt

	// 走过 20 分钟：剩余 40 分钟 > ttl/2，不续期。
	now = now.Add(20 * time.Minute)
	got, ok := m.Get(sess.ID)
	if !ok {
		t.Fatal("未过期应可取回")
	}
	if !got.ExpiresAt.Equal(originalExpiry) {
		t.Errorf("剩余时间充足时不应续期: %v → %v", originalExpiry, got.ExpiresAt)
	}

	// 再走 20 分钟：剩余 20 分钟 < ttl/2，触发滑动续期。
	now = now.Add(20 * time.Minute)
	got, ok = m.Get(sess.ID)
	if !ok {
		t.Fatal("未过期应可取回")
	}
	want := now.Add(ttl)
	if !got.ExpiresAt.Equal(want) {
		t.Errorf("应滑动续期到 %v，实际 %v", want, got.ExpiresAt)
	}

	// 超过有效期：返回 false 并删除。
	now = now.Add(2 * ttl)
	if _, ok := m.Get(sess.ID); ok {
		t.Error("已过期会话应返回 false")
	}
	if m.Len() != 0 {
		t.Errorf("过期会话应被清除，Len 实际 %d", m.Len())
	}
}

func TestSessionDestroy(t *testing.T) {
	m := NewSessionManager(time.Hour)
	sess, err := m.Create(PublicUser{Username: "admin", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}

	m.Destroy(sess.ID)
	if _, ok := m.Get(sess.ID); ok {
		t.Error("销毁后不应再取到会话")
	}
	m.Destroy("never-existed") // 幂等，不 panic
	m.Destroy("")
}

func TestSessionDestroyUser(t *testing.T) {
	m := NewSessionManager(time.Hour)
	mustCreate := func(username string) Session {
		t.Helper()
		s, err := m.Create(PublicUser{Username: username, Role: RoleViewer})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	a1 := mustCreate("alice")
	a2 := mustCreate("alice")
	b := mustCreate("bob")

	if n := m.DestroyUser("alice"); n != 2 {
		t.Errorf("期望清理 2 个会话，实际 %d", n)
	}
	if _, ok := m.Get(a1.ID); ok {
		t.Error("alice 的会话应已清除")
	}
	if _, ok := m.Get(a2.ID); ok {
		t.Error("alice 的会话应已清除")
	}
	if _, ok := m.Get(b.ID); !ok {
		t.Error("bob 的会话不应受影响")
	}
	if n := m.DestroyUser("alice"); n != 0 {
		t.Errorf("重复清理应返回 0，实际 %d", n)
	}
}

func TestSessionPurge(t *testing.T) {
	ttl := time.Hour
	m := NewSessionManager(ttl)
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, err := m.Create(PublicUser{Username: "admin", Role: RoleAdmin}); err != nil {
			t.Fatal(err)
		}
	}
	if m.Len() != 3 {
		t.Fatalf("Len 期望 3，实际 %d", m.Len())
	}

	now = now.Add(30 * time.Minute)
	if _, err := m.Create(PublicUser{Username: "admin", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}

	// 走到 75 分钟：前 3 个已过期（10:30 创建的那批在 11:00 过期），最后 1 个仍有效。
	now = now.Add(45 * time.Minute)
	m.Purge()
	if m.Len() != 1 {
		t.Errorf("Purge 后 Len 期望 1，实际 %d", m.Len())
	}
	if len(m.sessions) != 1 {
		t.Errorf("过期条目应被物理删除，map 长度实际 %d", len(m.sessions))
	}
}

// TestSessionZeroTTLDefault 非法 TTL 应回落到 12 小时。
func TestSessionZeroTTLDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		m := NewSessionManager(ttl)
		if m.TTL() != 12*time.Hour {
			t.Errorf("TTL=%v 应回落到 12h，实际 %v", ttl, m.TTL())
		}
	}
}

// TestSessionConcurrent 并发读写会话不应 panic 或丢数据（配合 -race）。
func TestSessionConcurrent(t *testing.T) {
	m := NewSessionManager(time.Hour)
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := m.Create(PublicUser{Username: "user" + string(rune('a'+i%5)), Role: RoleViewer})
			if err != nil {
				t.Errorf("Create 失败: %v", err)
				return
			}
			m.Get(s.ID)
			m.Len()
			if i%2 == 0 {
				m.Destroy(s.ID)
			}
			m.DestroyUser("usera")
			m.Purge()
		}(i)
	}
	wg.Wait()

	if n := m.Len(); n < 0 || n > 20 {
		t.Errorf("Len 越界: %d", n)
	}
}
