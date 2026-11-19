// Package dingtalk 实现钉钉自定义机器人渠道适配器（含加签）。
package dingtalk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/model"
)

// Config 是钉钉机器人配置。
type Config struct {
	// Webhook 是机器人 webhook 地址（已带 access_token）。
	Webhook string
	// Secret 是加签密钥；为空表示机器人未开启加签。
	Secret string
	// Timeout 是单次请求超时，默认 10s。
	Timeout time.Duration
	// Now 可在测试中替换时钟，默认 time.Now。
	Now func() time.Time
}

// Sender 是钉钉渠道适配器。
type Sender struct {
	cfg    Config
	client *http.Client
}

// New 创建钉钉适配器。
func New(cfg Config) (*Sender, error) {
	cfg.Webhook = strings.TrimSpace(cfg.Webhook)
	if cfg.Webhook == "" {
		return nil, fmt.Errorf("dingtalk: webhook 为空")
	}
	u, err := url.Parse(cfg.Webhook)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("dingtalk: webhook 不是合法 URL: %q", cfg.Webhook)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Sender{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Name 实现 channel.Sender。
func (s *Sender) Name() string { return "dingtalk" }

// Sign 计算钉钉加签：base64(HMAC-SHA256(secret, timestamp+"\n"+secret))。
func Sign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Send 实现 channel.Sender。
func (s *Sender) Send(ctx context.Context, msg *model.Message) error {
	endpoint, err := url.Parse(s.cfg.Webhook)
	if err != nil {
		return fmt.Errorf("dingtalk: 解析 webhook: %w", err)
	}
	if s.cfg.Secret != "" {
		timestamp := strconv.FormatInt(s.cfg.Now().UnixMilli(), 10)
		q := endpoint.Query()
		q.Set("timestamp", timestamp)
		q.Set("sign", Sign(s.cfg.Secret, timestamp))
		endpoint.RawQuery = q.Encode()
	}

	payload := map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": contentFor(msg),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("dingtalk: 序列化请求体: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("dingtalk: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("dingtalk: HTTP %d: %s", resp.StatusCode, trimmed(body))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
			resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return channel.Permanent(err)
		}
		return err
	}

	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("dingtalk: 解析响应失败: %w", err)
	}
	if result.ErrCode != 0 {
		err := fmt.Errorf("dingtalk: errcode=%d errmsg=%s", result.ErrCode, result.ErrMsg)
		// 130101 是「发送太快」被限流，等一会儿可以再试；其余错误重试无意义。
		if result.ErrCode == 130101 {
			return err
		}
		return channel.Permanent(err)
	}
	return nil
}

// contentFor 生成钉钉 text 消息正文：标题、正文、链接各占一行。
func contentFor(msg *model.Message) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(msg.Title) != "" {
		parts = append(parts, msg.Title)
	}
	if strings.TrimSpace(msg.Body) != "" {
		parts = append(parts, msg.Body)
	}
	if strings.TrimSpace(msg.Link) != "" {
		parts = append(parts, msg.Link)
	}
	return strings.Join(parts, "\n")
}

func trimmed(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
