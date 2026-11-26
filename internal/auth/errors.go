// Package auth 提供本地账号体系：口令哈希、用户存储与会话管理。
//
// 三个子能力相互独立：
//   - 口令：PBKDF2-HMAC-SHA256 自实现（仅标准库），编码格式带算法与迭代数，便于日后升级参数；
//   - 用户：单文件 JSON 持久化，原子写入，权限 0600 / 目录 0700；
//   - 会话：进程内内存表，TTL 滑动续期，不落盘（进程重启即失效，单实例部署可接受）。
//
// 本包不感知 HTTP，也不引用 internal/web，便于单独测试。
package auth

import "errors"

// 存储与会话层的哨兵错误。调用方用 errors.Is 判断，不要依赖错误文案。
var (
	// ErrUserExists 表示用户名已被占用。
	ErrUserExists = errors.New("用户已存在")
	// ErrUserNotFound 表示按用户名查不到用户（管理操作使用）。
	ErrUserNotFound = errors.New("用户不存在")
	// ErrInvalidPassword 表示密码不正确。
	//
	// 登录场景下「用户不存在」也返回它，避免通过错误文案枚举用户名。
	ErrInvalidPassword = errors.New("密码不正确")
	// ErrWeakPassword 表示密码不满足长度或非空白要求。
	ErrWeakPassword = errors.New("密码强度不足")
	// ErrLastAdmin 表示该操作会让系统里再无可用管理员。
	ErrLastAdmin = errors.New("不能删除或降级最后一个管理员")
	// ErrInvalidRole 表示角色不是 admin / viewer。
	ErrInvalidRole = errors.New("非法角色")
	// ErrUserDisabled 表示账号已被禁用，禁止登录。
	ErrUserDisabled = errors.New("账号已禁用")
)
