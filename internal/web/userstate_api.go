package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"jobworkspace/internal/userdata"
)

// userStateBodyLimit 是 PUT /api/me/state 的请求体上限（256KB）。
//
// 状态本身是很小的结构（最多 5000 条收藏 + 5000 条备注），256KB 足够宽松，
// 同时挡住「一次性灌爆磁盘」的请求。
const userStateBodyLimit = 256 << 10

// 与 internal/userdata 的归一化上限保持一致，仅用于提前返回字段级错误。
const (
	maxUserStateFavorites = 5000
	maxUserStateKeys      = 5000
	maxUserStateNoteRunes = 500
)

// userStatePayload 是 PUT /api/me/state 的请求体。
//
// 只声明可写字段：version 与 updatedAt 一律忽略，由服务端重算；
// 契约外的键由 encoding/json 自然忽略。
type userStatePayload struct {
	Favorites []string          `json:"favorites"`
	Stages    map[string]string `json:"stages"`
	Notes     map[string]string `json:"notes"`
}

// handleGetMyState 返回当前登录用户的收藏与投递状态。
//
// 路由：GET /api/me/state。用户名只取自会话，不接受客户端传入。
// 用户尚无数据时返回空 State 与 200（不是 404）——「没有数据」与「没有登录」是两回事。
func (s *Server) handleGetMyState(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if s.userState == nil {
		s.log.Error("读取用户状态失败：用户状态存储未初始化", "username", sess.Username)
		writeError(w, http.StatusInternalServerError, "STATE_UNAVAILABLE", "用户状态存储不可用")
		return
	}

	st, err := s.userState.Load(sess.Username)
	if err != nil {
		s.log.Error("读取用户状态失败", "username", sess.Username, "err", err)
		writeError(w, http.StatusInternalServerError, "STATE_READ_FAILED", "读取用户状态失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": st})
}

// handlePutMyState 整体替换当前登录用户的收藏与投递状态。
//
// 路由：PUT /api/me/state。请求体即 State（version/updatedAt 被忽略），
// 成功 200 {"ok":true,"state":{...}}；类型错误 400；字段超限 422（带 fields）。
func (s *Server) handlePutMyState(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "请求体必须是 application/json")
		return
	}
	if s.userState == nil {
		s.log.Error("保存用户状态失败：用户状态存储未初始化", "username", sess.Username)
		writeError(w, http.StatusInternalServerError, "STATE_UNAVAILABLE", "用户状态存储不可用")
		return
	}

	// 多读 1 字节用于区分「刚好 256KB」与「超出 256KB」。
	body, err := io.ReadAll(io.LimitReader(r.Body, userStateBodyLimit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "读取请求体失败")
		return
	}
	if len(body) > userStateBodyLimit {
		writeValidationFailed(w, map[string]string{"body": "请求体不能超过 256KB"})
		return
	}

	var in userStatePayload
	if err := json.Unmarshal(body, &in); err != nil {
		// 类型不符（例如 favorites 传成字符串）与语法错误都归为 400：
		// 这不是「值越界」，而是客户端根本没按契约发请求。
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法的状态 JSON")
		return
	}

	if fields := validateUserState(in); len(fields) > 0 {
		writeValidationFailed(w, fields)
		return
	}

	next, err := s.userState.Save(sess.Username, userdata.State{
		Favorites: in.Favorites,
		Stages:    in.Stages,
		Notes:     in.Notes,
	})
	if err != nil {
		s.log.Error("保存用户状态失败", "username", sess.Username, "err", err)
		writeError(w, http.StatusInternalServerError, "STATE_WRITE_FAILED", "保存用户状态失败")
		return
	}

	s.log.Info("保存用户状态", "username", sess.Username,
		"favorites", len(next.Favorites), "stages", len(next.Stages), "notes", len(next.Notes))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": next})
}

// validateUserState 做「超限」类校验，返回 JSON 字段名 → 中文提示。
//
// 只拦真正的越界：重复收藏、非法阶段这类可安全归一化的问题交给 userdata.Store，
// 避免把「客户端多传了一条重复 ID」升级成 422。
func validateUserState(in userStatePayload) map[string]string {
	fields := make(map[string]string)

	// 先按去重后的有效条目计数，客户端重复提交同一 ID 不应被判超限。
	seen := make(map[string]struct{}, len(in.Favorites))
	for _, raw := range in.Favorites {
		if id := strings.TrimSpace(raw); id != "" {
			seen[id] = struct{}{}
		}
		if len(seen) > maxUserStateFavorites {
			fields["favorites"] = "收藏最多 5000 条"
			break
		}
	}

	if len(in.Stages) > maxUserStateKeys {
		fields["stages"] = "投递状态最多 5000 条"
	}
	if len(in.Notes) > maxUserStateKeys {
		fields["notes"] = "备注最多 5000 条"
	} else {
		for _, note := range in.Notes {
			if utf8.RuneCountInString(strings.TrimSpace(note)) > maxUserStateNoteRunes {
				fields["notes"] = "单条备注不能超过 500 字"
				break
			}
		}
	}

	return fields
}
