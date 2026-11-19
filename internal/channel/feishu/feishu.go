// Package feishu 实现飞书自定义机器人渠道适配器（含加签）。
package feishu

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

// 支持的机器人消息形态。
const (
	// MsgTypeText 是纯文本消息。
	MsgTypeText = "text"
	// MsgTypeCard 是消息卡片（interactive）。
	MsgTypeCard = "card"
)

const (
	// defaultTimeout 是单次请求默认超时。
	defaultTimeout = 10 * time.Second
	// maxRespBody 是读取响应体的上限，避免异常响应拖垮进程。
	maxRespBody = 64 << 10
)

// Config 是飞书自定义机器人配置。
type Config struct {
	// Webhook 是机器人 webhook 地址，必填且必须是 http(s) URL。
	Webhook string
	// Secret 是加签密钥；为空表示机器人未开启加签。
	Secret string
	// MsgType 是消息形态，"text" 或 "card"，空按 "text"。
	MsgType string
	// Timeout 是单次请求超时，默认 10s。
	Timeout time.Duration
	// Now 可在测试中替换时钟，默认 time.Now；加签 timestamp 用 Unix 秒。
	Now func() time.Time
}

// Sender 是飞书渠道适配器。
type Sender struct {
	cfg    Config
	client *http.Client
}

// New 创建飞书适配器，并归一化 MsgType 与默认值。
func New(cfg Config) (*Sender, error) {
	cfg.Webhook = strings.TrimSpace(cfg.Webhook)
	if cfg.Webhook == "" {
		return nil, fmt.Errorf("feishu: webhook 为空")
	}
	u, err := url.Parse(cfg.Webhook)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("feishu: webhook 不是合法的 http(s) URL: %q", cfg.Webhook)
	}

	cfg.MsgType = strings.ToLower(strings.TrimSpace(cfg.MsgType))
	if cfg.MsgType == "" {
		cfg.MsgType = MsgTypeText
	}
	switch cfg.MsgType {
	case MsgTypeText, MsgTypeCard:
	default:
		return nil, fmt.Errorf("feishu: 不支持的 msg_type %q（可选 text/card）", cfg.MsgType)
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Sender{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Name 实现 channel.Sender。
func (s *Sender) Name() string { return "feishu" }

// Sign 计算飞书加签：base64.StdEncoding(HMAC-SHA256(key = timestamp+"\n"+secret, msg = ""))。
func Sign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Send 实现 channel.Sender。
func (s *Sender) Send(ctx context.Context, msg *model.Message) error {
	if msg == nil {
		return channel.Permanent(fmt.Errorf("feishu: 消息为空"))
	}

	payload := s.buildPayload(msg)

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("feishu: 序列化请求体: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Webhook, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("feishu: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		// 网络错误、超时等属于可重试失败。
		return fmt.Errorf("feishu: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("feishu: HTTP %d: %s", resp.StatusCode, trimmed(body))
		// 4xx 多为 webhook/密钥/参数错误，重试无意义；408/429 除外。
		if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
			resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return channel.Permanent(err)
		}
		return err
	}

	if err := checkResult(body); err != nil {
		return err
	}
	return nil
}

// buildPayload 组装请求体，包含可选的加签字段。
func (s *Sender) buildPayload(msg *model.Message) map[string]any {
	var payload map[string]any
	if s.cfg.MsgType == MsgTypeCard {
		payload = map[string]any{
			"msg_type": "interactive",
			"card":     cardFor(msg),
		}
	} else {
		payload = map[string]any{
			"msg_type": "text",
			"content": map[string]any{
				"text": textFor(msg),
			},
		}
	}

	if s.cfg.Secret != "" {
		timestamp := strconv.FormatInt(s.cfg.Now().Unix(), 10)
		payload["timestamp"] = timestamp
		payload["sign"] = Sign(s.cfg.Secret, timestamp)
	}
	return payload
}

// checkResult 解析飞书响应体并按错误码分类。
//
// 新版本返回 {"code":0,...}，部分老版本返回 {"StatusCode":0,...}，两者都兼容；
// 只有 HTTP 200 才会走到这里。
func checkResult(body []byte) error {
	var result struct {
		Code          *int   `json:"code"`
		StatusCode    *int   `json:"StatusCode"`
		Msg           string `json:"msg"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// 非 JSON 的 200 响应通常是网关异常页，重试有机会成功。
		return fmt.Errorf("feishu: 解析响应失败: %w", err)
	}

	var code int
	switch {
	case result.Code != nil:
		code = *result.Code
	case result.StatusCode != nil:
		code = *result.StatusCode
	default:
		// 没有任何 code 字段视为成功，保持向前兼容。
		return nil
	}
	if code == 0 {
		return nil
	}

	message := result.Msg
	if message == "" {
		message = result.StatusMessage
	}
	err := fmt.Errorf("feishu: code=%d msg=%s", code, message)
	if retryableCode(code) {
		return err
	}
	return channel.Permanent(err)
}

// retryableCode 报告飞书错误码是否值得重试。
//
// 9499 是自定义机器人限流（Too Many Requests），等一会儿可以再试；
// 其余错误码（签名错误、webhook 失效、参数非法等）重试无意义。
func retryableCode(code int) bool {
	switch code {
	case 9499:
		return true
	default:
		return false
	}
}

// textFor 生成飞书 text 消息正文：标题、正文、链接各占一行，空行省略。
func textFor(msg *model.Message) string {
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

// cardFor 生成飞书消息卡片：header 放标题与级别配色，elements 放正文和链接。
func cardFor(msg *model.Message) map[string]any {
	title := strings.TrimSpace(msg.Title)
	if title == "" {
		title = "通知"
	}
	body := strings.TrimSpace(msg.Body)
	link := strings.TrimSpace(msg.Link)
	if body == "" && link == "" {
		// 卡片 elements 不能为空，标题兜底作为正文，避免发出空卡片。
		body = title
	}

	elements := make([]any, 0, 2)
	if body != "" {
		elements = append(elements, map[string]any{
			"tag": "div",
			"text": map[string]any{
				"tag":     "lark_md",
				"content": body,
			},
		})
	}
	if link != "" {
		elements = append(elements, map[string]any{
			"tag": "action",
			"actions": []any{
				map[string]any{
					"tag":  "button",
					"text": map[string]any{"tag": "plain_text", "content": "查看详情"},
					"type": "default",
					"url":  link,
				},
			},
		})
	}

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": templateFor(msg.Level),
		},
		"elements": elements,
	}
}

// templateFor 把平台级别映射到飞书卡片配色。
func templateFor(level string) string {
	switch level {
	case model.LevelWarn:
		return "orange"
	case model.LevelError:
		return "red"
	default:
		return "blue"
	}
}

func trimmed(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
