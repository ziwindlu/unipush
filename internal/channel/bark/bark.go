// Package bark 实现 Bark（iOS 推送）渠道适配器。
package bark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/model"
)

// Config 是 Bark 渠道配置。
type Config struct {
	// ServerURL 是 Bark 服务地址，默认 https://api.day.app，自建服务可改。
	ServerURL string
	// DeviceKey 是 Bark App 里的设备 key，只能来自环境变量，不入库。
	DeviceKey string
	// Group 是 Bark 客户端上的消息分组名。
	Group string
	// Sound 是可选的提示音。
	Sound string
	// Timeout 是单次请求超时，默认 10s。
	Timeout time.Duration
}

// Sender 是 Bark 渠道适配器。
type Sender struct {
	cfg    Config
	client *http.Client
}

// New 创建 Bark 适配器。
func New(cfg Config) (*Sender, error) {
	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	if cfg.ServerURL == "" {
		cfg.ServerURL = "https://api.day.app"
	}
	if strings.TrimSpace(cfg.DeviceKey) == "" {
		return nil, fmt.Errorf("bark: device key 为空")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Sender{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Name 实现 channel.Sender。
func (s *Sender) Name() string { return "bark" }

// pushPayload 是 Bark POST /push 的请求体。
type pushPayload struct {
	DeviceKey string `json:"device_key"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body"`
	Level     string `json:"level,omitempty"`
	URL       string `json:"url,omitempty"`
	Group     string `json:"group,omitempty"`
	Sound     string `json:"sound,omitempty"`
}

// Send 实现 channel.Sender。
func (s *Sender) Send(ctx context.Context, msg *model.Message) error {
	payload := pushPayload{
		DeviceKey: s.cfg.DeviceKey,
		Title:     msg.Title,
		Body:      msg.Body,
		Level:     levelFor(msg.Level),
		URL:       msg.Link,
		Group:     s.cfg.Group,
		Sound:     s.cfg.Sound,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bark: 序列化请求体: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.ServerURL+"/push", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("bark: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("bark: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("bark: HTTP %d: %s", resp.StatusCode, trimmed(body))
		// 4xx 通常是 device key 错误或参数问题，重试没有意义；408/429 除外。
		if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
			resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return channel.Permanent(err)
		}
		return err
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("bark: 解析响应失败: %w", err)
	}
	if result.Code != 0 && result.Code != 200 {
		return channel.Permanent(fmt.Errorf("bark: code=%d message=%s", result.Code, result.Message))
	}
	return nil
}

// levelFor 把平台级别映射到 Bark 的通知级别。
func levelFor(level string) string {
	switch level {
	case model.LevelError:
		return "critical"
	case model.LevelWarn:
		return "timeSensitive"
	default:
		return "active"
	}
}

func trimmed(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
