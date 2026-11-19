// Package auth 提供密码哈希与会话 / API Token 的生成、校验原语。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// APITokenPrefix 是 API Token 的可见前缀，便于在日志与配置里认出它。
const APITokenPrefix = "up_"

// HashPassword 用 bcrypt 生成密码哈希。
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: 生成密码哈希: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword 校验密码是否匹配哈希。
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// NewToken 生成一个随机 token 及其 SHA-256 哈希。
// 原始 token 只返回给调用方展示一次，数据库只保存哈希。
func NewToken(prefix string) (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("auth: 生成随机 token: %w", err)
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashToken(token), nil
}

// HashToken 返回 token 的 SHA-256 十六进制表示，用作数据库主键。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EqualToken 以常量时间比较两个 token。
func EqualToken(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// GeneratePassword 生成可直接展示给用户的随机密码。
func GeneratePassword(length int) (string, error) {
	if length < 8 {
		length = 8
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: 生成随机密码: %w", err)
	}
	// base64 里去掉容易混淆的字符，保证复制粘贴不出错。
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, length)
	for i := range out {
		out[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(out), nil
}

// TokenPrefix 返回用于列表展示的 token 前缀（不含敏感部分）。
func TokenPrefix(token string) string {
	const keep = len(APITokenPrefix) + 6
	if len(token) <= keep {
		return token
	}
	return token[:keep] + "…"
}
