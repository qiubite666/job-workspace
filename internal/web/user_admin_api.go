package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"jobworkspace/internal/auth"
)

// 账号管理接口的请求体上限与字段规则，与 auth 包保持一致。
const (
	userAdminBodyLimit = 64 << 10
	userNameMinLen     = 3
	userNameMaxLen     = 32
	userPasswordMinLen = 8
	userPasswordMaxLen = 128
)

// userNamePattern 与 auth 包的用户名字符集一致；此处只用于把错误归到具体字段。
var userNamePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// createUserRequest 是 POST /api/admin/users 的请求体。
//
// Password 省略或为空表示由服务端生成 16 位随机密码，并通过响应里的
// generatedPassword 一次性返回。
type createUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Password    string `json:"password"`
}

// setPasswordRequest 是 POST /api/admin/users/{username}/password 的请求体。
type setPasswordRequest struct {
	Password string `json:"password"`
}

// handleAdminListUsers 返回全部后台账号。
//
// 只返回公开字段（绝不含密码哈希），按用户名升序。
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		s.log.Error("读取账号列表失败：认证组件未初始化")
		writeError(w, http.StatusInternalServerError, "AUTH_UNAVAILABLE", "账号服务不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"users": s.users.List(),
	})
}

// handleAdminCreateUser 新建账号。
//
// 成功 201；请求体非法 400；字段校验失败/用户名重复 422；
// 密码留空时随机生成 16 位并仅在本次响应的 generatedPassword 中返回。
func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, userAdminBodyLimit))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON")
		return
	}
	if s.users == nil {
		s.log.Error("创建账号失败：认证组件未初始化")
		writeError(w, http.StatusInternalServerError, "AUTH_UNAVAILABLE", "账号服务不可用")
		return
	}

	fields := make(map[string]string)

	// 与 auth 包相同的规范化：trim + 转小写；前端可直接填小写用户名。
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if msg := validateUserName(username); msg != "" {
		fields["username"] = msg
	}

	role := auth.RoleViewer
	if raw := strings.TrimSpace(req.Role); raw != "" {
		parsed, err := auth.ParseRole(raw)
		if err != nil {
			fields["role"] = "角色只能是 admin 或 viewer"
		} else {
			role = parsed
		}
	}

	password := req.Password
	generated := false
	if password == "" {
		pw, err := newRandomPassword()
		if err != nil {
			s.log.Error("生成随机密码失败", "err", err)
			writeError(w, http.StatusInternalServerError, "PASSWORD_GEN_FAILED", "生成初始密码失败")
			return
		}
		password, generated = pw, true
	} else if msg := validateUserPassword(password); msg != "" {
		fields["password"] = msg
	}

	if len(fields) > 0 {
		writeValidationFailed(w, fields)
		return
	}

	user, err := s.users.Create(username, req.DisplayName, role, password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrUserExists):
			writeValidationFailed(w, map[string]string{"username": "用户名已被占用"})
		case errors.Is(err, auth.ErrWeakPassword):
			writeValidationFailed(w, map[string]string{"password": err.Error()})
		case errors.Is(err, auth.ErrInvalidRole):
			writeValidationFailed(w, map[string]string{"role": "角色只能是 admin 或 viewer"})
		default:
			s.log.Error("创建账号失败", "username", username, "err", err)
			writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "创建账号失败")
		}
		return
	}

	s.log.Info("后台新增账号", "username", user.Username, "role", string(user.Role), "generated", generated)
	resp := map[string]any{"ok": true, "user": user}
	if generated {
		resp["generatedPassword"] = password
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleAdminSetPassword 重置指定账号的密码。
//
// 成功 200；用户不存在 404；密码长度不足 422；
// 密码留空时随机生成 16 位并仅在本次响应的 generatedPassword 中返回。
func (s *Server) handleAdminSetPassword(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
	if username == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "账号不存在")
		return
	}
	if s.users == nil {
		s.log.Error("修改密码失败：认证组件未初始化")
		writeError(w, http.StatusInternalServerError, "AUTH_UNAVAILABLE", "账号服务不可用")
		return
	}

	var req setPasswordRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, userAdminBodyLimit))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON")
		return
	}

	if _, ok := s.users.Get(username); !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "账号不存在")
		return
	}

	password := req.Password
	generated := false
	if password == "" {
		pw, err := newRandomPassword()
		if err != nil {
			s.log.Error("生成随机密码失败", "err", err)
			writeError(w, http.StatusInternalServerError, "PASSWORD_GEN_FAILED", "生成新密码失败")
			return
		}
		password, generated = pw, true
	} else if msg := validateUserPassword(password); msg != "" {
		writeValidationFailed(w, map[string]string{"password": msg})
		return
	}

	if err := s.users.SetPassword(username, password); err != nil {
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "账号不存在")
		case errors.Is(err, auth.ErrWeakPassword):
			writeValidationFailed(w, map[string]string{"password": err.Error()})
		default:
			s.log.Error("重置密码失败", "username", username, "err", err)
			writeError(w, http.StatusInternalServerError, "STORE_WRITE_FAILED", "修改密码失败")
		}
		return
	}

	s.log.Info("后台重置账号密码", "username", username, "generated", generated)
	resp := map[string]any{"ok": true}
	if generated {
		resp["generatedPassword"] = password
	}
	writeJSON(w, http.StatusOK, resp)
}

// validateUserName 按 auth 包规则校验用户名，返回空串表示通过。
func validateUserName(name string) string {
	if name == "" {
		return "用户名不能为空"
	}
	if n := utf8.RuneCountInString(name); n < userNameMinLen || n > userNameMaxLen {
		return "用户名长度需为 " + strconv.Itoa(userNameMinLen) + "~" + strconv.Itoa(userNameMaxLen) + " 位"
	}
	if !userNamePattern.MatchString(name) {
		return "用户名只能包含小写字母、数字、点、下划线与连字符"
	}
	return ""
}

// validateUserPassword 按 auth 包规则校验密码，返回空串表示通过。
func validateUserPassword(password string) string {
	n := utf8.RuneCountInString(password)
	if n < userPasswordMinLen {
		return "密码至少 " + strconv.Itoa(userPasswordMinLen) + " 位"
	}
	if n > userPasswordMaxLen {
		return "密码不能超过 " + strconv.Itoa(userPasswordMaxLen) + " 位"
	}
	if strings.TrimSpace(password) == "" {
		return "密码不能全为空白字符"
	}
	return ""
}

// newRandomPassword 生成 16 位十六进制随机密码（8 字节 crypto/rand）。
func newRandomPassword() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
