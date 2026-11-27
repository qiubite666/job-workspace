package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"jobworkspace/internal/model"
)

// maxImportJobs 限制单次导入条数，避免一个请求把内存打满。
const maxImportJobs = 50000

// libraryClearWord 是清空职位库的确认词。
const libraryClearWord = "CLEAR"

// handleMyCredentials 返回当前账号的上报令牌与 cookie 配置状态。
//
// 令牌只返回给令牌的主人（会话鉴权），并且不写日志。
// cookie 永远不回传：前端只需要知道"配没配"。
func (s *Server) handleMyCredentials(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "凭据存储不可用")
		return
	}

	token, err := s.creds.Ensure(sess.Username)
	if err != nil {
		s.log.Error("初始化账号令牌失败", "err", err, "user", sess.Username)
		writeError(w, http.StatusInternalServerError, "CREDENTIAL_FAILED", "无法初始化上报令牌")
		return
	}

	c, _ := s.creds.Get(sess.Username)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"username":         sess.Username,
		"token":            token,
		"cookieConfigured": strings.TrimSpace(c.Cookie) != "",
		"updatedAt":        c.UpdatedAt,
	})
}

// handleRotateMyToken 重新生成当前账号的上报令牌。
func (s *Server) handleRotateMyToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "凭据存储不可用")
		return
	}

	token, err := s.creds.Rotate(sess.Username)
	if err != nil {
		s.log.Error("重置令牌失败", "err", err, "user", sess.Username)
		writeError(w, http.StatusInternalServerError, "CREDENTIAL_FAILED", "重置令牌失败")
		return
	}

	s.log.Warn("账号重置了上报令牌", "user", sess.Username, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token})
}

// handlePutMyCookie 保存当前账号的采集 cookie。
func (s *Server) handlePutMyCookie(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "凭据存储不可用")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法的 JSON")
		return
	}

	if err := s.creds.SetCookie(sess.Username, req.Cookie); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "COOKIE_INVALID", err.Error())
		return
	}

	s.log.Info("账号更新了采集 cookie", "user", sess.Username, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"cookieConfigured": strings.TrimSpace(req.Cookie) != "",
	})
}

// handleDeleteMyCookie 清除当前账号的 cookie。
func (s *Server) handleDeleteMyCookie(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "凭据存储不可用")
		return
	}
	if err := s.creds.ClearCookie(sess.Username); err != nil {
		writeError(w, http.StatusInternalServerError, "CREDENTIAL_FAILED", "清除 cookie 失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cookieConfigured": false})
}

// handleExportMyLibrary 导出当前账号的职位库为 JSON 附件。
func (s *Server) handleExportMyLibrary(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.lib == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "职位库不可用")
		return
	}

	jobs, _, err := s.lib.Load(sess.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_READ_FAILED", "读取职位库失败")
		return
	}

	payload := map[string]any{
		"version":    1,
		"username":   sess.Username,
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"count":      len(jobs),
		"jobs":       jobs,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EXPORT_FAILED", "导出失败")
		return
	}

	name := fmt.Sprintf("jobs-%s-%s.json", sess.Username, time.Now().Format("20060102-1504"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleImportMyLibrary 把 JSON 导入到当前账号的职位库（合并，不覆盖）。
func (s *Server) handleImportMyLibrary(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.lib == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "职位库不可用")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var req struct {
		Jobs  []model.Job `json:"jobs"`
		Items []model.Job `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "导入文件不是合法的 JSON")
		return
	}
	jobs := req.Jobs
	if len(jobs) == 0 {
		jobs = req.Items
	}
	if len(jobs) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "NO_ITEMS", "文件里没有可导入的岗位")
		return
	}
	if len(jobs) > maxImportJobs {
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_MANY",
			fmt.Sprintf("单次最多导入 %d 条", maxImportJobs))
		return
	}

	// 导入数据来自用户自己的文件，但要防止缺 ID 的条目把库搅乱。
	valid := make([]model.Job, 0, len(jobs))
	skipped := 0
	for _, j := range jobs {
		if strings.TrimSpace(j.ID) == "" {
			skipped++
			continue
		}
		valid = append(valid, j)
	}
	if len(valid) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "NO_ITEMS", "所有条目都缺少 id，无法导入")
		return
	}

	added, total, err := s.lib.Merge(sess.Username, valid)
	if err != nil {
		s.log.Error("导入职位失败", "err", err, "user", sess.Username)
		writeError(w, http.StatusInternalServerError, "IMPORT_FAILED", "导入失败")
		return
	}

	s.log.Info("账号导入职位", "user", sess.Username, "imported", len(valid), "added", added, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"imported": len(valid),
		"added":    added,
		"skipped":  skipped,
		"total":    total,
	})
}

// handleClearMyLibrary 清空当前账号的职位库。
//
// 只影响调用者自己的库：其他账号的文件不会被触碰。
func (s *Server) handleClearMyLibrary(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.lib == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "职位库不可用")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法的 JSON")
		return
	}
	if strings.ToUpper(strings.TrimSpace(req.Confirm)) != libraryClearWord {
		writeError(w, http.StatusUnprocessableEntity, "CONFIRM_REQUIRED",
			"需要显式确认：请求体中的 confirm 必须为 "+libraryClearWord)
		return
	}

	removed, err := s.lib.Clear(sess.Username)
	if err != nil {
		s.log.Error("清空职位库失败", "err", err, "user", sess.Username)
		writeError(w, http.StatusInternalServerError, "CLEAR_FAILED", "清空失败，请重试")
		return
	}

	s.log.Warn("账号清空了自己的职位库", "user", sess.Username, "removed", removed, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"cleared": removed,
		"at":      time.Now().UTC().Format(time.RFC3339),
	})
}

// pendingJDItem 是待补 JD 的最小信息集。
type pendingJDItem struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
	SecurityID string `json:"securityId"`
	JobName    string `json:"jobName"`
}

// handlePendingJD 返回当前账号"缺 JD 且具备抓取条件"的岗位。
//
// 既支持会话鉴权，也支持上报令牌：扩展后台没有登录 Cookie，
// 但它手里有该账号的令牌，用令牌反查账号即可。
func (s *Server) handlePendingJD(w http.ResponseWriter, r *http.Request) {
	username := ""
	if sess, ok := s.currentSession(r); ok {
		username = sess.Username
	} else if u, ok := s.resolveIngestUser(r); ok {
		username = u
	}
	if username == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.lib == nil {
		writeError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "职位库不可用")
		return
	}

	limit := 30
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}

	jobs, _, err := s.lib.Load(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_READ_FAILED", "读取职位库失败")
		return
	}

	items := make([]pendingJDItem, 0, limit)
	pending := 0
	for _, j := range jobs {
		if strings.TrimSpace(j.Description) != "" {
			continue
		}
		// 没有 securityId 的记录抓不到（BOSS 会返回空页面），不算进待补。
		if strings.TrimSpace(j.SecurityID) == "" || strings.TrimSpace(j.ExternalID) == "" {
			continue
		}
		pending++
		if len(items) < limit {
			items = append(items, pendingJDItem{
				ID:         j.ID,
				ExternalID: j.ExternalID,
				SecurityID: j.SecurityID,
				JobName:    j.JobName,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"total": pending,
		"items": items,
	})
}
