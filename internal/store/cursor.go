package store

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// EncodeCursor 把分页位置编码成不透明字符串，避免调用方依赖内部列。
func EncodeCursor(c MessageCursor) string {
	raw := fmt.Sprintf("%d:%s", c.CreatedAt.UTC().UnixMilli(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 解析 EncodeCursor 生成的分页游标。
func DecodeCursor(encoded string) (*MessageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	millis, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" {
		return nil, fmt.Errorf("cursor 格式不正确")
	}
	var n int64
	if _, err := fmt.Sscanf(millis, "%d", &n); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, fmt.Errorf("cursor 时间无效")
	}
	return &MessageCursor{CreatedAt: time.UnixMilli(n).UTC(), ID: id}, nil
}
