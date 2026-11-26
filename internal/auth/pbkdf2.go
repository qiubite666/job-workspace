package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	// algorithmPBKDF2 是编码串里的算法标识，同时决定校验时选用的 PRF。
	algorithmPBKDF2 = "pbkdf2-sha256"
	// defaultIterations 是生产环境使用的迭代数，约百毫秒量级。
	defaultIterations = 210_000
	// defaultSaltLen 是随机盐长度（字节）。
	defaultSaltLen = 16
	// defaultKeyLen 是派生的密钥长度（字节）。
	defaultKeyLen = 32
	// maxIterations 是校验时可接受的迭代数上限，防止恶意哈希触发 CPU 拒绝服务。
	maxIterations = 1_000_000
)

// HashPassword 用生产参数（PBKDF2-HMAC-SHA256，210000 轮，16 字节盐，32 字节密钥）
// 计算口令哈希，返回形如 `pbkdf2-sha256$210000$<saltBase64>$<hashBase64>` 的字符串。
//
// 盐来自 crypto/rand，同一口令每次调用结果都不同。
func HashPassword(password string) (string, error) {
	return HashPasswordWith(password, defaultIterations, defaultSaltLen, defaultKeyLen)
}

// HashPasswordWith 与 HashPassword 相同，但允许指定迭代数、盐长度与密钥长度。
//
// 仅供测试与调参使用：调低迭代数会显著削弱安全性，生产路径必须走 HashPassword。
func HashPasswordWith(password string, iterations, saltLen, keyLen int) (string, error) {
	if iterations < 1 || iterations > maxIterations {
		return "", fmt.Errorf("迭代数 %d 超出允许范围 [1,%d]", iterations, maxIterations)
	}
	if saltLen < 8 || saltLen > 64 {
		return "", fmt.Errorf("盐长度 %d 超出允许范围 [8,64]", saltLen)
	}
	if keyLen < 16 || keyLen > 64 {
		return "", fmt.Errorf("密钥长度 %d 超出允许范围 [16,64]", keyLen)
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}

	key := PBKDF2Key([]byte(password), salt, iterations, keyLen)
	return fmt.Sprintf("%s$%d$%s$%s",
		algorithmPBKDF2,
		iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 校验 password 是否匹配 encoded。
//
// 任何异常（格式非法、算法未知、base64 损坏、迭代数越界）都返回 false 而不是 panic；
// 哈希比较使用 crypto/subtle 恒定时间比较，避免时序侧信道。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != algorithmPBKDF2 {
		return false
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > maxIterations {
		return false
	}

	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}

	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false
	}

	got := PBKDF2Key([]byte(password), salt, iterations, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// PBKDF2Key 按 RFC 2898 派生密钥：
//
//	DK = T1 || T2 || ... || Tn，Ti = F(P, S, c, i)，
//	F 为 U1..Uc 的逐字节异或，U1 = PRF(P, S || INT(i))，Uj = PRF(P, U(j-1))。
//
// PRF 固定为 HMAC-SHA256（与 algorithmPBKDF2 一致）。参数非法时返回 nil 或降级处理，
// 不 panic：iter < 1 视为 1，keyLen <= 0 返回 nil。
func PBKDF2Key(password, salt []byte, iter, keyLen int) []byte {
	if keyLen <= 0 {
		return nil
	}
	if iter < 1 {
		iter = 1
	}

	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	var counter [4]byte

	for block := 1; block <= blocks; block++ {
		// U1 = PRF(P, S || INT(i))
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		prf.Write(counter[:])
		u = prf.Sum(u[:0])

		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
