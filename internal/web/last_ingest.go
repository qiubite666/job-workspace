package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LastIngest 记录"最近一次成功上报来自哪个账号"。
//
// 存在的意义只有一个：账号之间是隔离的，但浏览器扩展里只配一把令牌。
// 切账号时忘了换令牌，数据会静默写进另一个账号，页面上完全看不出来。
// 有了这条记录，工作台就能提示"你正在看的库不是刚刚上报的那个账号"。
type LastIngest struct {
	Username string `json:"username"`
	Kind     string `json:"kind"` // list | detail
	Platform string `json:"platform"`
	At       string `json:"at"` // RFC3339
}

// lastIngestFile 是记录文件名，落在数据目录下，权限 0600。
const lastIngestFile = "last-ingest.json"

var lastIngestMu sync.Mutex

// recordIngest 记录一次成功上报；失败只打日志，不影响上报本身。
func (s *Server) recordIngest(username, kind, platform string) {
	if s.st == nil {
		return
	}

	rec := LastIngest{
		Username: username,
		Kind:     kind,
		Platform: platform,
		At:       time.Now().UTC().Format(time.RFC3339),
	}

	lastIngestMu.Lock()
	defer lastIngestMu.Unlock()

	path := filepath.Join(s.st.Dir(), lastIngestFile)
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		s.log.Warn("记录上报来源失败", "err", err)
	}
}

// lastIngest 读取最近一次上报来源；没有记录时返回零值。
func (s *Server) lastIngest() LastIngest {
	if s.st == nil {
		return LastIngest{}
	}

	lastIngestMu.Lock()
	defer lastIngestMu.Unlock()

	raw, err := os.ReadFile(filepath.Join(s.st.Dir(), lastIngestFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("读取上报来源失败", "err", err)
		}
		return LastIngest{}
	}

	var rec LastIngest
	if err := json.Unmarshal(raw, &rec); err != nil {
		return LastIngest{}
	}
	return rec
}
