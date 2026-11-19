// Package email 实现 SMTP 邮件渠道适配器。
//
// 只依赖标准库，支持三种连接方式：starttls（默认，服务器支持才升级）、
// ssl（隐式 TLS）、none（明文，内网中继常见）。
package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/model"
)

// 加密方式取值。
const (
	tlsStartTLS = "starttls" // 普通连接后通过 STARTTLS 升级，默认值
	tlsSSL      = "ssl"      // 建立连接即 TLS（隐式 TLS，常见 465 端口）
	tlsNone     = "none"     // 不加密，仅建议内网中继使用
)

// Config 是邮件渠道配置。
type Config struct {
	// Host 是 SMTP 服务器地址，必填。
	Host string
	// Port 是 SMTP 端口，0 或空按 587 处理。
	Port int
	// Username 登录用户名，可空；内网中继通常不需要认证。
	Username string
	// Password 登录密码，仅在 Username 非空时使用。
	Password string
	// From 是发件人地址，必填。
	From string
	// To 是收件人地址列表，至少一个。
	To []string
	// TLS 是加密方式：starttls（默认）/ ssl / none。
	TLS string
	// SubjectPrefix 是主题前缀，例如 "[unipush]"，为空则直接用标题。
	SubjectPrefix string
	// Timeout 是单次投递的超时，默认 10s。
	Timeout time.Duration
}

// Sender 是邮件渠道适配器。
type Sender struct {
	cfg Config
}

// New 创建邮件渠道适配器，并校验必填参数。
func New(cfg Config) (*Sender, error) {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.From = strings.TrimSpace(cfg.From)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.SubjectPrefix = strings.TrimSpace(cfg.SubjectPrefix)

	if cfg.Host == "" {
		return nil, fmt.Errorf("email: host 不能为空")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("email: from 不能为空")
	}
	// 地址会直接写进 SMTP 命令与邮件头，禁止换行以避免头注入。
	if strings.ContainsAny(cfg.Host, "\r\n") || strings.ContainsAny(cfg.From, "\r\n") {
		return nil, fmt.Errorf("email: host / from 不能包含换行")
	}

	to := make([]string, 0, len(cfg.To))
	for _, addr := range cfg.To {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if strings.ContainsAny(addr, "\r\n") {
			return nil, fmt.Errorf("email: to 不能包含换行")
		}
		to = append(to, addr)
	}
	if len(to) == 0 {
		return nil, fmt.Errorf("email: 至少需要一个收件人")
	}
	cfg.To = to

	switch strings.ToLower(strings.TrimSpace(cfg.TLS)) {
	case "":
		cfg.TLS = tlsStartTLS
	case tlsStartTLS, tlsSSL, tlsNone:
		cfg.TLS = strings.ToLower(strings.TrimSpace(cfg.TLS))
	default:
		return nil, fmt.Errorf("email: tls 只能是 starttls / ssl / none，当前是 %q", cfg.TLS)
	}

	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("email: port 必须在 1-65535 之间，当前是 %d", cfg.Port)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}

	return &Sender{cfg: cfg}, nil
}

// Name 实现 channel.Sender。
func (s *Sender) Name() string { return "email" }

// Send 实现 channel.Sender。
//
// 发送流程：建连（必要时 TLS）→ SMTP 握手 → 可选 STARTTLS → 可选认证 →
// MAIL FROM / RCPT TO / DATA / QUIT。所有步骤的错误都经过 classifyErr 分类：
// 5xx 不可重试，4xx 与网络、超时、TLS 错误可重试。
func (s *Sender) Send(ctx context.Context, msg *model.Message) error {
	if msg == nil {
		return channel.Permanent(fmt.Errorf("email: 消息为空"))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("email: 上下文已取消: %w", err)
	}

	raw, err := buildMessage(s.cfg, msg)
	if err != nil {
		return fmt.Errorf("email: 构造邮件: %w", err)
	}

	conn, err := s.dial(ctx)
	if err != nil {
		return classifyErr(err)
	}
	// 给整个 SMTP 会话设置截止时间，避免服务器不响应时永久阻塞。
	_ = conn.SetDeadline(deadlineFor(ctx, s.cfg.Timeout))

	// 上下文取消时主动关闭连接，让阻塞中的读写立刻返回。
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopped:
		}
	}()

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return classifyErr(fmt.Errorf("email: SMTP 握手失败: %w", err))
	}
	defer c.Close()

	// starttls 模式下只有服务器声明支持才升级；ssl 模式在 dial 阶段已完成 TLS。
	if s.cfg.TLS == tlsStartTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
			if err := c.StartTLS(tlsCfg); err != nil {
				return classifyErr(fmt.Errorf("email: STARTTLS 失败: %w", err))
			}
		}
	}

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return classifyErr(fmt.Errorf("email: 认证失败: %w", err))
		}
	}

	if err := c.Mail(s.cfg.From); err != nil {
		return classifyErr(fmt.Errorf("email: MAIL FROM 被拒绝: %w", err))
	}
	for _, rcpt := range s.cfg.To {
		if err := c.Rcpt(rcpt); err != nil {
			return classifyErr(fmt.Errorf("email: RCPT TO %s 被拒绝: %w", rcpt, err))
		}
	}

	w, err := c.Data()
	if err != nil {
		return classifyErr(fmt.Errorf("email: DATA 被拒绝: %w", err))
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return classifyErr(fmt.Errorf("email: 写入邮件正文失败: %w", err))
	}
	if err := w.Close(); err != nil {
		return classifyErr(fmt.Errorf("email: 结束 DATA 失败: %w", err))
	}
	if err := c.Quit(); err != nil {
		return classifyErr(fmt.Errorf("email: QUIT 失败: %w", err))
	}
	return nil
}

// dial 按配置建立到 SMTP 服务器的连接。
func (s *Sender) dial(ctx context.Context) (net.Conn, error) {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	if s.cfg.TLS == tlsSSL {
		dialer := &net.Dialer{Timeout: s.cfg.Timeout}
		tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("email: 建立 TLS 连接 %s 失败: %w", addr, err)
		}
		return conn, nil
	}

	conn, err := net.DialTimeout("tcp", addr, s.cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("email: 连接 %s 失败: %w", addr, err)
	}
	return conn, nil
}

// classifyErr 把 SMTP 服务器返回的 5xx 归为不可重试错误，
// 其余（4xx、网络、超时、TLS、认证握手异常）保持可重试。
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 500 && tp.Code < 600 {
		return channel.Permanent(err)
	}
	return err
}

// deadlineFor 取「本次投递超时」与「上下文截止时间」中更早的一个。
func deadlineFor(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

// buildMessage 生成完整的 RFC 5322 邮件（头部 + multipart/alternative 正文）。
func buildMessage(cfg Config, msg *model.Message) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	plain, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"8bit"},
	})
	if err != nil {
		return nil, fmt.Errorf("创建 text/plain 部分: %w", err)
	}
	if _, err := plain.Write([]byte(renderText(msg))); err != nil {
		return nil, fmt.Errorf("写入 text/plain 部分: %w", err)
	}

	htmlPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/html; charset=utf-8"},
		"Content-Transfer-Encoding": {"8bit"},
	})
	if err != nil {
		return nil, fmt.Errorf("创建 text/html 部分: %w", err)
	}
	if _, err := htmlPart.Write([]byte(renderHTML(msg))); err != nil {
		return nil, fmt.Errorf("写入 text/html 部分: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("结束 multipart 正文: %w", err)
	}

	subject := msg.Title
	if cfg.SubjectPrefix != "" {
		subject = cfg.SubjectPrefix + " " + msg.Title
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", sanitizeHeader(cfg.From))
	fmt.Fprintf(&buf, "To: %s\r\n", sanitizeHeader(strings.Join(cfg.To, ", ")))
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", sanitizeHeader(subject)))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	buf.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n", mw.Boundary())
	buf.WriteString("\r\n")
	buf.Write(body.Bytes())
	return buf.Bytes(), nil
}

// renderText 生成纯文本正文。
func renderText(msg *model.Message) string {
	var b strings.Builder
	if msg.Title != "" {
		b.WriteString(msg.Title)
		b.WriteString("\n\n")
	}
	b.WriteString(msg.Body)
	if msg.Link != "" {
		b.WriteString("\n\n")
		b.WriteString(msg.Link)
	}
	b.WriteString("\n")
	return b.String()
}

// renderHTML 生成 HTML 正文，标题、正文与链接都做 HTML 转义。
func renderHTML(msg *model.Message) string {
	title := html.EscapeString(sanitizeHeader(msg.Title))
	body := html.EscapeString(msg.Body)
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "<br>\n")

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", title)
	b.WriteString("</head>\n<body>\n")
	fmt.Fprintf(&b, "<p>%s</p>\n", title)
	fmt.Fprintf(&b, "<p>%s</p>\n", body)
	if msg.Link != "" {
		link := html.EscapeString(msg.Link)
		fmt.Fprintf(&b, "<p><a href=\"%s\">%s</a></p>\n", link, link)
	}
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

// sanitizeHeader 去掉邮件头里不允许出现的换行。
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
