package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"jobworkspace/internal/auth"
	"jobworkspace/internal/model"
	"jobworkspace/internal/store"
)

// migrateLegacy 把旧版本的单库数据接管给某个账号。
//
// v3.x 之前职位库和上报令牌都是全局的（<data>/jobs.json、<data>/ingest.token）。
// 改成分账号后必须一次性迁移，否则升级当天所有人都会看到空库。
// 归属规则：最早的 admin；没有 admin 则取第一个账号。
func (s *Server) migrateLegacy(log *slog.Logger) {
	if s.st == nil || s.lib == nil || s.creds == nil || s.users == nil {
		return
	}

	users := s.users.List()
	if len(users) == 0 {
		return
	}

	owner := ""
	for _, u := range users {
		if u.Role == auth.RoleAdmin {
			owner = u.Username
			break
		}
	}
	if owner == "" {
		owner = users[0].Username
	}

	dataDir := s.st.Dir()

	if n, err := s.lib.AdoptLegacy(filepath.Join(dataDir, store.JobsFile), owner); err != nil {
		log.Error("迁移历史职位库失败", "err", err, "user", owner)
	} else if n > 0 {
		log.Warn("已将历史职位库迁移到账号", "user", owner, "jobs", n)
	}

	// 旧的全局 cookie 文件同样接管给 owner，否则升级后该账号的采集会突然失效。
	if p := strings.TrimSpace(s.cookieFile); p != "" {
		if raw, err := os.ReadFile(p); err == nil {
			if cookie := strings.TrimSpace(string(raw)); cookie != "" {
				if cur, _ := s.creds.Get(owner); strings.TrimSpace(cur.Cookie) == "" {
					if err := s.creds.SetCookie(owner, cookie); err != nil {
						log.Error("接管全局 cookie 失败", "err", err, "user", owner)
					} else {
						log.Warn("已将全局 cookie 接管到账号", "user", owner)
					}
				}
			}
		}
	}

	if ok, err := s.creds.AdoptLegacyToken(filepath.Join(dataDir, "ingest.token"), owner); err != nil {
		log.Error("迁移历史上报令牌失败", "err", err, "user", owner)
	} else if ok {
		log.Warn("已将历史上报令牌接管到账号", "user", owner)
	}
}

// userCookie 返回该账号自己的采集 cookie。
//
// 刻意不回退到启动参数 --cookie-file：那是一个全局文件，回退等于让没有
// 配置 cookie 的账号借用别人的登录态，直接破坏账号隔离。旧部署的全局
// cookie 会在迁移时被接管到 owner 账号（见 migrateLegacy）。
func (s *Server) userCookie(username string) string {
	if s.creds == nil {
		return ""
	}
	c, err := s.creds.Get(username)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Cookie)
}

// readCollectedJobs 读取采集子进程写到临时文件的结果。
func readCollectedJobs(path string) ([]model.Job, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var jobs []model.Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}
