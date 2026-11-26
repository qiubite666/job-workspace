// Package errs 定义统一的错误码与平台异常归一化。
//
// 设计目标：让「异常处理」从各平台模块里抽离出来。平台适配器只需抛 PlatformError
// 或原始 error，编排层就能用同一套规则判断「该不该重试 / 该不该降级 / 该不该停止翻页」。
package errs

import (
	"errors"
	"fmt"
	"strings"
)

// Code 是归一化后的错误码。
type Code string

const (
	CodeNetwork          Code = "NETWORK"           // 网络错误，可重试
	CodeTimeout          Code = "TIMEOUT"           // 超时，可重试
	CodeAuthRequired     Code = "AUTH_REQUIRED"     // 需要登录态 / cookie 失效
	CodeRateLimited      Code = "RATE_LIMITED"      // 触发限频，可退避重试
	CodeBlocked          Code = "BLOCKED"           // 风控拦截 / 验证码，应停止
	CodeRobotsDisallowed Code = "ROBOTS_DISALLOWED" // robots.txt 禁止，应停止
	CodeParseFailed      Code = "PARSE_FAILED"      // 响应结构不符预期
	CodeEmpty            Code = "EMPTY"             // 空结果（非错误，但应停止翻页）
	CodeBadConfig        Code = "BAD_CONFIG"        // 配置缺失或非法
	CodeUnknown          Code = "UNKNOWN"           // 未归类
)

// PlatformError 是跨平台统一错误。
type PlatformError struct {
	Code      Code
	Msg       string
	Retryable bool
	Status    int
	Platform  string
	Err       error
}

// Error 实现 error 接口。
func (e *PlatformError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	if e.Platform != "" {
		b.WriteString("[" + e.Platform + "] ")
	}
	b.WriteString(string(e.Code))
	if e.Status > 0 {
		fmt.Fprintf(&b, "(%d)", e.Status)
	}
	if e.Msg != "" {
		b.WriteString(": " + e.Msg)
	}
	if e.Err != nil {
		b.WriteString(" <- " + e.Err.Error())
	}
	return b.String()
}

// Unwrap 支持 errors.Is / errors.As。
func (e *PlatformError) Unwrap() error { return e.Err }

// New 构造一个 PlatformError，默认按错误码推断是否可重试。
func New(code Code, format string, args ...any) *PlatformError {
	return &PlatformError{
		Code:      code,
		Msg:       fmt.Sprintf(format, args...),
		Retryable: isRetryableCode(code),
	}
}

// Wrap 包装底层 error。
func Wrap(code Code, err error, format string, args ...any) *PlatformError {
	return &PlatformError{
		Code:      code,
		Msg:       fmt.Sprintf(format, args...),
		Retryable: isRetryableCode(code),
		Err:       err,
	}
}

// ClassifyStatus 把 HTTP 状态码归一为错误码；无法归类返回 false。
func ClassifyStatus(status int) (Code, bool) {
	switch {
	case status == 401 || status == 403:
		return CodeAuthRequired, true
	case status == 429:
		return CodeRateLimited, true
	case status >= 500:
		return CodeNetwork, true
	case status >= 400:
		return CodeUnknown, true
	}
	return "", false
}

// ClassifyPayload 根据业务响应体判断风控/限频/登录失效。
//
// BOSS 直聘常见：code 31/37 + message「您的环境存在异常」；
// 限频文案：「您操作太频繁」「访问频繁」「请稍后再试」；
// 登录失效：「请先登录」「登录后查看」。
func ClassifyPayload(codeValue any, message string) (Code, bool) {
	code := ""
	switch v := codeValue.(type) {
	case string:
		code = strings.TrimSpace(v)
	case float64:
		code = fmt.Sprintf("%.0f", v)
	case int:
		code = fmt.Sprintf("%d", v)
	case nil:
	default:
		code = fmt.Sprint(v)
	}

	switch code {
	case "31", "37":
		return CodeBlocked, true
	case "17", "35":
		return CodeAuthRequired, true
	}

	msg := strings.ToLower(message)
	switch {
	case msg == "":
		return "", false
	case strings.Contains(msg, "环境存在异常"), strings.Contains(msg, "安全校验"),
		strings.Contains(msg, "验证码"), strings.Contains(msg, "verify"):
		return CodeBlocked, true
	case strings.Contains(msg, "频繁"), strings.Contains(msg, "稍后再试"),
		strings.Contains(msg, "too many"), strings.Contains(msg, "rate limit"):
		return CodeRateLimited, true
	case strings.Contains(msg, "登录"), strings.Contains(msg, "login"),
		strings.Contains(msg, "unauthorized"):
		return CodeAuthRequired, true
	}
	return "", false
}

// isRetryableCode 判定错误码是否值得重试。
func isRetryableCode(code Code) bool {
	switch code {
	case CodeNetwork, CodeTimeout, CodeRateLimited:
		return true
	}
	return false
}

// IsRetryable 判断任意 error 是否可重试。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *PlatformError
	if errors.As(err, &pe) {
		return pe.Retryable || isRetryableCode(pe.Code)
	}
	return false
}

// Info 提取任意 error 的错误码、消息与可重试性；不会 panic。
func Info(err error) (Code, string, bool) {
	if err == nil {
		return "", "", false
	}
	var pe *PlatformError
	if errors.As(err, &pe) {
		return pe.Code, pe.Msg, pe.Retryable || isRetryableCode(pe.Code)
	}
	return CodeUnknown, err.Error(), false
}

// As 是 errors.As 的便捷包装。
func As(err error) (*PlatformError, bool) {
	var pe *PlatformError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
