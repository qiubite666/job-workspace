package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// resetConfirmToken 是清空数据的确认口令。
//
// 光靠 admin 权限还不够：清空是不可恢复操作，要求请求体显式带上这个值，
// 避免一个误发的 POST（或手工 curl 少打一个参数）就把数据抹掉。
const resetConfirmToken = "CLEAR_ALL"

// resetBodyLimit 限制请求体，这个接口本来只该收到一个几十字节的 JSON。
const resetBodyLimit = 4 << 10

type resetRequest struct {
	Confirm string `json:"confirm"`
}

// handleAdminReset 清空全部业务数据。
//
// 范围：岗位库 + 全部账号的收藏/投递/备注 + 最近一次采集报告。
// **不删账号**：账号属于配置而非业务数据，清掉会把人锁在门外。
func (s *Server) handleAdminReset(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "数据存储不可用")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, resetBodyLimit)
	var req resetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法的 JSON")
		return
	}
	if strings.TrimSpace(req.Confirm) != resetConfirmToken {
		writeError(w, http.StatusUnprocessableEntity, "CONFIRM_REQUIRED",
			"需要显式确认：请求体中的 confirm 必须为 "+resetConfirmToken)
		return
	}

	who := "unknown"
	if sess, ok := s.currentSession(r); ok {
		who = sess.Username
	}

	// 职位库改为分账号存放，这里逐个账号清空；账号本身保留。
	removedJobs := 0
	if s.lib != nil && s.users != nil {
		for _, u := range s.users.List() {
			n, err := s.lib.Clear(u.Username)
			if err != nil {
				s.log.Error("清空账号职位库失败", "err", err, "user", u.Username, "by", who)
				writeError(w, http.StatusInternalServerError, "CLEAR_FAILED", "清空职位库失败，请重试")
				return
			}
			removedJobs += n
		}
	}
	// 兼容：升级前遗留的全局库也一并清掉，避免它被再次迁移回某个账号。
	if s.st != nil {
		if n, err := s.st.ClearJobs(); err == nil {
			removedJobs += n
		}
	}

	removedStates := 0
	if s.userState != nil {
		var err error
		removedStates, err = s.userState.ClearAll()
		if err != nil {
			// 岗位已清掉，这里失败属于部分成功，必须如实告诉调用方。
			s.log.Error("清空用户数据失败", "err", err, "by", who)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok":      false,
				"error":   "PARTIAL_CLEAR",
				"message": "岗位数据已清空，但用户收藏/投递数据未能完全清除，请重试",
				"cleared": map[string]int{
					"jobs":       removedJobs,
					"userStates": removedStates,
				},
			})
			return
		}
	}

	s.log.Warn("管理员清空了所有业务数据",
		"by", who, "jobs", removedJobs, "userStates", removedStates,
		"ip", clientIP(r))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"cleared": map[string]int{
			"jobs":       removedJobs,
			"userStates": removedStates,
		},
		"at": time.Now().UTC().Format(time.RFC3339),
	})
}
