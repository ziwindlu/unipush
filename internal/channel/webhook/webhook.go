// Package webhook 实现通用 HTTP webhook 渠道适配器。
//
// 它不预设目标服务的字段约定：调用方可以用 BodyTemplate 把消息渲染成任意
// 请求体；留空时直接发送 model.Message 的 JSON 表示，方便自建服务直接消费。
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/model"
)

// defaultContentType 是未配置 ContentType 时使用的请求体类型。
const defaultContentType = "application/json; charset=utf-8"

// Config 是通用 webhook 渠道配置。
type Config struct {
	// URL 是目标地址，必填，只支持 http(s)。
	URL string
	// Method 是 HTTP 方法，空值按 POST 处理。
	Method string
	// Headers 是附加请求头，在 Content-Type 之后设置，因此可以覆盖它。
	Headers map[string]string
	// ContentType 是请求体类型，空值按 application/json; charset=utf-8。
	ContentType string
	// BodyTemplate 是 Go text/template 模板；留空时直接发送消息的 JSON。
	BodyTemplate string
	// Timeout 是单次请求超时，默认 10s。
	Timeout time.Duration
}

// templateData 是 BodyTemplate 的渲染数据。
//
// 内嵌 *model.Message 让 {{.Title}}、{{.Body}}、{{.Level}}、{{.Source}}、
// {{.Link}}、{{.DedupeKey}}、{{.ID}} 等字段直接可用；JSON 是整条消息的
// JSON 编码（便于模板把原始消息整体嵌进请求体），Extra 是消息的扩展字段。
type templateData struct {
	*model.Message
	JSON  string
	Extra map[string]any
}

// Sender 是通用 webhook 渠道适配器。
type Sender struct {
	cfg    Config
	client *http.Client
	// tmpl 是在 New 中预解析并缓存的请求体模板；BodyTemplate 为空时为 nil。
	tmpl *template.Template
}

// New 创建通用 webhook 适配器。
func New(cfg Config) (*Sender, error) {
	cfg.URL = strings.TrimSpace(cfg.URL)
	if cfg.URL == "" {
		return nil, fmt.Errorf("webhook: url 为空")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") || u.Host == "" {
		return nil, fmt.Errorf("webhook: url 不是合法的 http(s) 地址: %q", cfg.URL)
	}

	cfg.Method = strings.ToUpper(strings.TrimSpace(cfg.Method))
	if cfg.Method == "" {
		cfg.Method = http.MethodPost
	}
	// 试构造一次请求，直接复用标准库对方法合法性的校验，避免自己维护白名单。
	if _, err := http.NewRequest(cfg.Method, cfg.URL, nil); err != nil {
		return nil, fmt.Errorf("webhook: 非法 HTTP 方法 %q: %w", cfg.Method, err)
	}

	cfg.ContentType = strings.TrimSpace(cfg.ContentType)
	if cfg.ContentType == "" {
		cfg.ContentType = defaultContentType
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}

	s := &Sender{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
	if strings.TrimSpace(cfg.BodyTemplate) != "" {
		// missingkey=error：模板引用了不存在的 map 键时直接报错，而不是静默渲染成空值。
		// 模板解析只在 New 里做一次并缓存，Send 只做执行。
		tmpl, err := template.New("webhook-body").Option("missingkey=error").Parse(cfg.BodyTemplate)
		if err != nil {
			return nil, fmt.Errorf("webhook: 解析 body 模板: %w", err)
		}
		s.tmpl = tmpl
	}
	return s, nil
}

// Name 实现 channel.Sender。
func (s *Sender) Name() string { return "webhook" }

// Send 实现 channel.Sender。
func (s *Sender) Send(ctx context.Context, msg *model.Message) error {
	if msg == nil {
		return channel.Permanent(fmt.Errorf("webhook: 消息为空"))
	}
	body, err := s.bodyFor(msg)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, s.cfg.Method, s.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return channel.Permanent(fmt.Errorf("webhook: 构造请求: %w", err))
	}
	// 先设 Content-Type，再应用配置的请求头，让 Headers 可以覆盖它。
	req.Header.Set("Content-Type", s.cfg.ContentType)
	for k, v := range s.cfg.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// 网络错误、超时等属于可重试失败。
		return fmt.Errorf("webhook: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	err = fmt.Errorf("webhook: HTTP %d: %s", resp.StatusCode, trimmed(respBody))
	// 408/429 是临时状况，5xx 是服务端问题，都可以重试；
	// 其余 4xx 是地址、鉴权或参数问题，重试没有意义。
	if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
		resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		return channel.Permanent(err)
	}
	return err
}

// bodyFor 生成请求体：配置了模板就渲染模板，否则发送消息本身的 JSON。
func (s *Sender) bodyFor(msg *model.Message) ([]byte, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		// 消息里带了无法序列化的 Extra 值时，重试也不会成功。
		return nil, channel.Permanent(fmt.Errorf("webhook: 序列化消息: %w", err))
	}
	if s.tmpl == nil {
		return raw, nil
	}

	var buf bytes.Buffer
	data := templateData{Message: msg, JSON: string(raw), Extra: msg.Extra}
	if err := s.tmpl.Execute(&buf, data); err != nil {
		// 模板执行错误（缺字段、类型不匹配等）属于配置问题，不可重试。
		return nil, channel.Permanent(fmt.Errorf("webhook: 渲染 body 模板: %w", err))
	}
	return buf.Bytes(), nil
}

// trimmed 把响应体压缩成一行并截断到 300 个字符，避免错误信息过长。
func trimmed(body []byte) string {
	s := strings.TrimSpace(string(body))
	runes := []rune(s)
	if len(runes) > 300 {
		return string(runes[:300]) + "..."
	}
	return s
}
