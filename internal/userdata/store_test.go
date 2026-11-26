package userdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// newTestStore 创建一个指向临时目录的仓库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "userstate"))
}

// TestNewState 确认空状态的形状：版本 1、容器非 nil。
func TestNewState(t *testing.T) {
	st := NewState()
	if st.Version != fileVersion {
		t.Fatalf("Version = %d，期望 %d", st.Version, fileVersion)
	}
	if st.Favorites == nil || st.Stages == nil || st.Notes == nil {
		t.Fatalf("空状态不应含有 nil 容器: %+v", st)
	}
	if len(st.Favorites) != 0 || len(st.Stages) != 0 || len(st.Notes) != 0 {
		t.Fatalf("空状态应为空: %+v", st)
	}
	if st.UpdatedAt != "" {
		t.Fatalf("空状态的 UpdatedAt 应为空，实际 %q", st.UpdatedAt)
	}
}

// TestLoadMissingFile 文件不存在视为空状态，不是错误。
func TestLoadMissingFile(t *testing.T) {
	s := newTestStore(t)

	st, err := s.Load("alice")
	if err != nil {
		t.Fatalf("缺失文件不应报错: %v", err)
	}
	if st.Version != fileVersion || len(st.Favorites) != 0 || len(st.Stages) != 0 || len(st.Notes) != 0 {
		t.Fatalf("缺失文件应返回空状态，实际 %+v", st)
	}
	if _, statErr := os.Stat(s.Path("alice")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Load 不应创建文件，stat 结果: %v", statErr)
	}
}

// TestSaveLoadRoundTrip 验证保存后原样读回，且版本与时间由服务端补齐。
func TestSaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)

	in := State{
		Version:   99, // 客户端乱传的版本应被覆盖
		Favorites: []string{"manual:abc123", "boss:xyz"},
		Stages:    map[string]string{"manual:abc123": "applied", "boss:xyz": "interview"},
		Notes:     map[string]string{"manual:abc123": "约了下周三面试"},
		UpdatedAt: "1999-01-01T00:00:00Z", // 应被重算
	}

	saved, err := s.Save("Alice", in)
	if err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if saved.Version != fileVersion {
		t.Fatalf("Version = %d，期望 %d", saved.Version, fileVersion)
	}
	if saved.UpdatedAt == in.UpdatedAt || saved.UpdatedAt == "" {
		t.Fatalf("UpdatedAt 应由服务端重算，实际 %q", saved.UpdatedAt)
	}
	ts, err := time.Parse(time.RFC3339, saved.UpdatedAt)
	if err != nil {
		t.Fatalf("UpdatedAt 应为 RFC3339: %v", err)
	}
	if ts.Location() != time.UTC {
		t.Fatalf("UpdatedAt 应为 UTC，实际时区 %v", ts.Location())
	}

	// 用户名大小写与空白应被规范化，读写落到同一个文件。
	got, err := s.Load("  ALICE  ")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !equalStrings(got.Favorites, saved.Favorites) {
		t.Fatalf("Favorites 往返不一致: %v vs %v", got.Favorites, saved.Favorites)
	}
	if len(got.Stages) != 2 || got.Stages["manual:abc123"] != "applied" || got.Stages["boss:xyz"] != "interview" {
		t.Fatalf("Stages 往返不一致: %v", got.Stages)
	}
	if got.Notes["manual:abc123"] != "约了下周三面试" {
		t.Fatalf("Notes 往返不一致: %v", got.Notes)
	}
	if got.UpdatedAt != saved.UpdatedAt {
		t.Fatalf("UpdatedAt 往返不一致: %q vs %q", got.UpdatedAt, saved.UpdatedAt)
	}

	// 落盘文件的 JSON 字段必须与契约同名同形。
	raw, err := os.ReadFile(s.Path("alice"))
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	var disk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("落盘文件不是合法 JSON: %v", err)
	}
	for _, key := range []string{"version", "favorites", "stages", "notes", "updatedAt"} {
		if _, ok := disk[key]; !ok {
			t.Fatalf("落盘文件缺少字段 %q: %s", key, raw)
		}
	}
}

// TestSaveNormalizes 逐项验证契约 §2.1 的归一化规则。
func TestSaveNormalizes(t *testing.T) {
	t.Run("重复与空白收藏", func(t *testing.T) {
		s := newTestStore(t)
		saved, err := s.Save("bob", State{
			Favorites: []string{"a:1", " ", "a:1", "  a:2  ", "", "a:2"},
		})
		if err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		want := []string{"a:1", "a:2"}
		if !equalStrings(saved.Favorites, want) {
			t.Fatalf("Favorites = %v，期望 %v", saved.Favorites, want)
		}
	})

	t.Run("非法阶段归一为空", func(t *testing.T) {
		s := newTestStore(t)
		saved, err := s.Save("bob", State{
			Stages: map[string]string{
				"a:1": " APPLIED ",
				"a:2": "unknown",
				"a:3": "",
				"":    "applied",
			},
		})
		if err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		if got := saved.Stages["a:1"]; got != "applied" {
			t.Fatalf("大小写与空白应被归一，实际 %q", got)
		}
		if got := saved.Stages["a:2"]; got != "" {
			t.Fatalf("非法阶段应归一为 \"\"，实际 %q", got)
		}
		if got, ok := saved.Stages["a:3"]; !ok || got != "" {
			t.Fatalf("空阶段应保留键且值为 \"\"，实际 %q (存在=%v)", got, ok)
		}
		if _, ok := saved.Stages[""]; ok {
			t.Fatalf("空岗位 ID 应被丢弃: %v", saved.Stages)
		}
	})

	t.Run("超长备注按 rune 截断", func(t *testing.T) {
		s := newTestStore(t)
		long := strings.Repeat("面", maxNoteRunes+50)
		saved, err := s.Save("bob", State{Notes: map[string]string{"a:1": long}})
		if err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		note := saved.Notes["a:1"]
		if n := utf8.RuneCountInString(note); n != maxNoteRunes {
			t.Fatalf("备注长度 = %d，期望 %d", n, maxNoteRunes)
		}
		if !strings.HasPrefix(long, note) {
			t.Fatalf("截断结果应为原文前缀")
		}
		if !utf8.ValidString(note) {
			t.Fatalf("截断不得破坏 UTF-8")
		}
	})

	t.Run("超量收藏截断", func(t *testing.T) {
		s := newTestStore(t)
		in := make([]string, 0, maxFavorites+10)
		for i := 0; i < maxFavorites+10; i++ {
			in = append(in, fmt.Sprintf("p:%04d", i))
		}
		saved, err := s.Save("bob", State{Favorites: in})
		if err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		if len(saved.Favorites) != maxFavorites {
			t.Fatalf("收藏条数 = %d，期望 %d", len(saved.Favorites), maxFavorites)
		}
		if saved.Favorites[0] != "p:0000" {
			t.Fatalf("截断应保留插入顺序，首项 = %q", saved.Favorites[0])
		}
	})

	t.Run("超量 stages 与 notes 截断", func(t *testing.T) {
		s := newTestStore(t)
		stages := make(map[string]string, maxStageKeys+10)
		notes := make(map[string]string, maxNoteKeys+10)
		for i := 0; i < maxStageKeys+10; i++ {
			key := fmt.Sprintf("p:%04d", i)
			stages[key] = "applied"
			notes[key] = "备注"
		}
		saved, err := s.Save("bob", State{Stages: stages, Notes: notes})
		if err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		if len(saved.Stages) != maxStageKeys {
			t.Fatalf("stages 键数 = %d，期望 %d", len(saved.Stages), maxStageKeys)
		}
		if len(saved.Notes) != maxNoteKeys {
			t.Fatalf("notes 键数 = %d，期望 %d", len(saved.Notes), maxNoteKeys)
		}
		// 按键名升序截断：p:0000 ~ p:4999 应全部保留，p:5009 应被丢弃。
		if _, ok := saved.Stages["p:5009"]; ok {
			t.Fatalf("超限键应被丢弃")
		}
		if _, ok := saved.Notes["p:0000"]; !ok {
			t.Fatalf("排序靠前的键应被保留")
		}
	})

	t.Run("空备注被丢弃", func(t *testing.T) {
		s := newTestStore(t)
		saved, err := s.Save("bob", State{Notes: map[string]string{"a:1": "   "}})
		if err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
		if _, ok := saved.Notes["a:1"]; ok {
			t.Fatalf("空备注不应保留: %v", saved.Notes)
		}
	})
}

// TestPathTraversalRejected 覆盖契约要求的穿越用例，并确认没有产生任何文件。
func TestPathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "userstate")
	s := NewStore(dir)

	cases := []struct {
		name     string
		username string
	}{
		{"上级目录", "../x"},
		{"斜杠", "a/b"},
		{"反斜杠", `a\b`},
		{"纯上级路径", ".."},
		{"单点", "."},
		{"纯点", "..."},
		{"空串", ""},
		{"空白", "   "},
		{"过短", "ab"},
		{"超长", strings.Repeat("a", usernameMaxLen+1)},
		{"非法字符", "a b"},
		{"大写转小写后合法但原串带空格路径", "../admin"},
		{"绝对路径", "/etc/passwd"},
		{"Windows 盘符", "C:/windows"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := s.Path(tc.username); p != "" {
				t.Fatalf("Path(%q) = %q，期望空串", tc.username, p)
			}
			if _, err := s.Load(tc.username); !errors.Is(err, ErrInvalidUsername) {
				t.Fatalf("Load(%q) 错误 = %v，期望 ErrInvalidUsername", tc.username, err)
			}
			if _, err := s.Save(tc.username, NewState()); !errors.Is(err, ErrInvalidUsername) {
				t.Fatalf("Save(%q) 错误 = %v，期望 ErrInvalidUsername", tc.username, err)
			}
			if err := s.Delete(tc.username); !errors.Is(err, ErrInvalidUsername) {
				t.Fatalf("Delete(%q) 错误 = %v，期望 ErrInvalidUsername", tc.username, err)
			}
		})
	}

	// 目录里不应出现任何文件：所有调用在拼路径之前就失败了。
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("读取临时目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("穿越用例不应产生任何文件，实际 %v", entries)
	}
}

// TestPathStaysInsideDir 合法用户名拼出的路径必须位于仓库目录内。
func TestPathStaysInsideDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "userstate")
	s := NewStore(dir)

	got := s.Path("  Alice.01  ")
	want := filepath.Join(dir, "alice.01.json")
	if got != want {
		t.Fatalf("Path = %q，期望 %q", got, want)
	}
	if filepath.Dir(got) != dir {
		t.Fatalf("路径越界: %q 不在 %q 内", got, dir)
	}
}

// TestLoadCorruptFile 文件损坏必须报错，不能静默返回空状态。
func TestLoadCorruptFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"空文件", ""},
		{"空白文件", "   \n"},
		{"半截 JSON", `{"version":1,"favorites":[`},
		{"类型不符", `{"version":1,"favorites":"nope"}`},
		{"版本不支持", `{"version":2,"favorites":[]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "userstate")
			s := NewStore(dir)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("创建目录失败: %v", err)
			}
			path := s.Path("carol")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("写入损坏文件失败: %v", err)
			}

			st, err := s.Load("carol")
			if err == nil {
				t.Fatalf("损坏文件应报错，实际返回 %+v", st)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("错误应包含文件路径，实际 %v", err)
			}
		})
	}
}

// TestConcurrentAccess 并发读写不应 panic 或产生半截文件。
func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)

	const workers = 8
	const rounds = 20

	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			username := fmt.Sprintf("user%02d", i)
			for r := 0; r < rounds; r++ {
				id := fmt.Sprintf("job:%d-%d", i, r)
				want := fmt.Sprintf("备注 %d/%d", i, r)
				if _, err := s.Save(username, State{
					Favorites: []string{id},
					Stages:    map[string]string{id: "applied"},
					Notes:     map[string]string{id: want},
				}); err != nil {
					errCh <- fmt.Errorf("Save(%s) 失败: %w", username, err)
					return
				}
				st, err := s.Load(username)
				if err != nil {
					errCh <- fmt.Errorf("Load(%s) 失败: %w", username, err)
					return
				}
				if len(st.Favorites) != 1 {
					errCh <- fmt.Errorf("Load(%s) 收藏条数异常: %v", username, st.Favorites)
					return
				}
			}
		}(i)
	}

	// 同时观察同一个账号：读端只能看到完整快照。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			if _, err := s.Load("user00"); err != nil {
				errCh <- fmt.Errorf("并发读取失败: %w", err)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// 全部结束后，每个账号的最后一次写入都应完整可读。
	for i := 0; i < workers; i++ {
		username := fmt.Sprintf("user%02d", i)
		st, err := s.Load(username)
		if err != nil {
			t.Fatalf("最终读取 %s 失败: %v", username, err)
		}
		if len(st.Favorites) != 1 || len(st.Stages) != 1 || len(st.Notes) != 1 {
			t.Fatalf("%s 最终状态不完整: %+v", username, st)
		}
	}
}

// TestFilePermissions 落盘后目录 0700、文件 0600（Windows 无 POSIX 权限位，跳过）。
func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 POSIX 权限位")
	}

	dir := filepath.Join(t.TempDir(), "userstate")
	s := NewStore(dir)
	if _, err := s.Save("dave", NewState()); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat 目录失败: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("目录权限 = %o，期望 0700", got)
	}

	fileInfo, err := os.Stat(s.Path("dave"))
	if err != nil {
		t.Fatalf("stat 文件失败: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("文件权限 = %o，期望 0600", got)
	}
}

// TestDeleteIdempotent 删除已存在文件与不存在的文件都应成功。
func TestDeleteIdempotent(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.Save("erin", NewState()); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if _, err := os.Stat(s.Path("erin")); err != nil {
		t.Fatalf("Save 后文件应存在: %v", err)
	}

	if err := s.Delete("erin"); err != nil {
		t.Fatalf("首次 Delete 失败: %v", err)
	}
	if err := s.Delete("erin"); err != nil {
		t.Fatalf("重复 Delete 应幂等，实际 %v", err)
	}

	st, err := s.Load("erin")
	if err != nil {
		t.Fatalf("删除后 Load 不应报错: %v", err)
	}
	if len(st.Favorites) != 0 {
		t.Fatalf("删除后应为空状态，实际 %+v", st)
	}
}

// TestNormalizeUsername 校验用户名规范化与拒绝规则的细节。
func TestNormalizeUsername(t *testing.T) {
	ok := []struct {
		in   string
		want string
	}{
		{"alice", "alice"},
		{"  Alice  ", "alice"},
		{"a.b_c-d", "a.b_c-d"},
		{"a123", "a123"},
	}
	for _, tc := range ok {
		got, err := normalizeUsername(tc.in)
		if err != nil {
			t.Fatalf("normalizeUsername(%q) 报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeUsername(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	bad := []string{"", "  ", "ab", "a b", "AB C", "a/b", `a\b`, "..", "...", "a:", "用户名"}
	for _, in := range bad {
		if _, err := normalizeUsername(in); !errors.Is(err, ErrInvalidUsername) {
			t.Fatalf("normalizeUsername(%q) 错误 = %v，期望 ErrInvalidUsername", in, err)
		}
	}
}

// TestEmptyDirRejected 未配置目录时不得把文件写到进程当前目录。
func TestEmptyDirRejected(t *testing.T) {
	s := NewStore("  ")
	if _, err := s.Save("alice", NewState()); !errors.Is(err, errEmptyDir) {
		t.Fatalf("Save 错误 = %v，期望 errEmptyDir", err)
	}
	if _, err := s.Load("alice"); !errors.Is(err, errEmptyDir) {
		t.Fatalf("Load 错误 = %v，期望 errEmptyDir", err)
	}
}

// equalStrings 比较两个字符串切片（顺序敏感）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
