// Package wecom 实现企业微信群机器人渠道适配器。
package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/model"
)

// maxContentBytes 是企业微信 markdown 消息内容的字节上限。
const maxContentBytes = 4096

// ellipsis 是内容被截断时追加的省略号（占 3 个字节）。
const ellipsis = "…"

// 消息类型。
const (
	msgTypeMarkdown = "markdown"
	msgTypeText     = "text"
)

// Config 是企业微信群机器人配置。
type Config struct {
	// Webhook 是群机器人 webhook 地址，形如
	// https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx。
	Webhook string
	// MsgType 是消息形态，只支持 markdown 或 text，空值按 markdown 处理。
	MsgType string
	// Timeout 是单次请求超时，默认 10s。
	Timeout time.Duration
}

// Sender 是企业微信渠道适配器。
type Sender struct {
	cfg    Config
	client *http.Client
}

// New 创建企业微信适配器。
func New(cfg Config) (*Sender, error) {
	cfg.Webhook = strings.TrimSpace(cfg.Webhook)
	if cfg.Webhook == "" {
		return nil, fmt.Errorf("wecom: webhook 为空")
	}
	u, err := url.Parse(cfg.Webhook)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("wecom: webhook 不是合法的 http(s) URL: %q", cfg.Webhook)
	}

	cfg.MsgType = strings.ToLower(strings.TrimSpace(cfg.MsgType))
	switch cfg.MsgType {
	case "":
		cfg.MsgType = msgTypeMarkdown
	case msgTypeMarkdown, msgTypeText:
	default:
		return nil, fmt.Errorf("wecom: 不支持的 msgtype %q，只支持 markdown 或 text", cfg.MsgType)
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Sender{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Name 实现 channel.Sender。
func (s *Sender) Name() string { return "wecom" }

// payload 是企业微信 webhook 的请求体；两种形态互斥，未使用的字段省略。
type payload struct {
	MsgType  string        `json:"msgtype"`
	Markdown *contentField `json:"markdown,omitempty"`
	Text     *contentField `json:"text,omitempty"`
}

type contentField struct {
	Content string `json:"content"`
}

// Send 实现 channel.Sender。
func (s *Sender) Send(ctx context.Context, msg *model.Message) error {
	body := payload{MsgType: s.cfg.MsgType}
	if s.cfg.MsgType == msgTypeText {
		body.Text = &contentField{Content: textContent(msg)}
	} else {
		body.Markdown = &contentField{Content: truncateContent(markdownContent(msg))}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("wecom: 序列化请求体: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Webhook, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("wecom: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		// 网络错误、超时等可以重试。
		return fmt.Errorf("wecom: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("wecom: HTTP %d: %s", resp.StatusCode, trimmed(respBody))
		// 408/429 是暂时性错误，5xx 是服务端问题，都可以重试；
		// 其余 4xx 是请求本身有问题，重试没有意义。
		if isRetryableStatus(resp.StatusCode) {
			return err
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return channel.Permanent(err)
		}
		return err
	}

	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("wecom: 解析响应失败: %w", err)
	}
	if result.ErrCode != 0 {
		err := fmt.Errorf("wecom: errcode=%d errmsg=%s", result.ErrCode, result.ErrMsg)
		if isRetryableErrCode(result.ErrCode) {
			return err
		}
		return channel.Permanent(err)
	}
	return nil
}

// markdownContent 生成企业微信 markdown 正文：
// 标题加粗、正文原样、链接转成「详情」，空字段整行省略。
func markdownContent(msg *model.Message) string {
	parts := make([]string, 0, 3)
	if title := strings.TrimSpace(msg.Title); title != "" {
		parts = append(parts, "**"+msg.Title+"**")
	}
	if body := strings.TrimSpace(msg.Body); body != "" {
		parts = append(parts, msg.Body)
	}
	if link := strings.TrimSpace(msg.Link); link != "" {
		parts = append(parts, "[详情]("+msg.Link+")")
	}
	return strings.Join(parts, "\n")
}

// textContent 生成企业微信 text 正文：标题与正文换行拼接，空字段省略。
func textContent(msg *model.Message) string {
	parts := make([]string, 0, 2)
	if title := strings.TrimSpace(msg.Title); title != "" {
		parts = append(parts, msg.Title)
	}
	if body := strings.TrimSpace(msg.Body); body != "" {
		parts = append(parts, msg.Body)
	}
	return strings.Join(parts, "\n")
}

// truncateContent 把内容截断到 maxContentBytes 字节以内（含省略号）。
// 截断点会回退到 UTF-8 字符边界，避免切出半个汉字。
func truncateContent(content string) string {
	if len(content) <= maxContentBytes {
		return content
	}
	// 预留省略号的 3 个字节，保证最终内容不超过上限。
	limit := maxContentBytes - len(ellipsis)
	for limit > 0 && !utf8.RuneStart(content[limit]) {
		limit--
	}
	return content[:limit] + ellipsis
}

// isRetryableStatus 报告 HTTP 状态码是否属于可重试的暂时性错误。
func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500
}

// isRetryableErrCode 报告企业微信 errcode 是否属于可重试的限流/系统繁忙错误。
func isRetryableErrCode(code int) bool {
	switch code {
	case -1, // 系统繁忙，稍后重试
		45009, // 接口调用超过限制
		45011, // API 调用太频繁
		45033, // 接口调用超过限制
		45047: // 客服接口调用超过限制
		return true
	default:
		return false
	}
}

func trimmed(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
