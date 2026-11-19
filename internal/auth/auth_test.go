package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("密码不能明文保存")
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("正确密码应校验通过")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("错误密码不应通过")
	}
}

func TestNewToken(t *testing.T) {
	raw, hash, err := NewToken(APITokenPrefix)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !strings.HasPrefix(raw, APITokenPrefix) {
		t.Fatalf("token %q 缺少前缀", raw)
	}
	if len(raw) < 40 {
		t.Fatalf("token 太短: %d", len(raw))
	}
	if hash != HashToken(raw) {
		t.Fatal("返回的哈希与 token 不匹配")
	}
	if hash == raw {
		t.Fatal("不能把明文当作哈希")
	}
	other, _, err := NewToken(APITokenPrefix)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if other == raw {
		t.Fatal("两次生成的 token 不应相同")
	}
}

func TestGeneratePassword(t *testing.T) {
	password, err := GeneratePassword(16)
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	if len(password) != 16 {
		t.Fatalf("长度 = %d, 期望 16", len(password))
	}
	for _, r := range password {
		if !strings.ContainsRune("abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789", r) {
			t.Fatalf("密码包含易混淆字符 %q", r)
		}
	}
}
