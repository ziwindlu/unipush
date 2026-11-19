package email_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/email"
	"github.com/ziwindlu/unipush/internal/model"
)

const testTimeout = 5 * time.Second

// fakeOptions 控制假 SMTP 服务器的响应。
type fakeOptions struct {
	// rcptCode 非 0 时，对 RCPT TO 返回该错误码（例如 550）。
	rcptCode int
	// authCode 非 0 时作为 AUTH 的响应码；默认 500，让误发 AUTH 的测试失败。
	authCode int
}

// fakeSMTP 是一个只实现最小 SMTP 协议、用于捕获邮件原文的本地服务器。
type fakeSMTP struct {
	ln     net.Listener
	opts   fakeOptions
	dataCh chan string

	mu       sync.Mutex
	mailFrom string
	rcpts    []string
	authSeen bool
}

// startFakeSMTP 在 127.0.0.1 的随机端口启动假服务器，测试结束自动关闭。
func startFakeSMTP(t *testing.T, opts fakeOptions) *fakeSMTP {
	t.Helper()
	if opts.authCode == 0 {
		opts.authCode = 500
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口: %v", err)
	}
	s := &fakeSMTP{ln: ln, opts: opts, dataCh: make(chan string, 1)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// port 返回假服务器监听的端口。
func (s *fakeSMTP) port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

// serve 处理单个连接的 SMTP 会话。
func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = w.WriteString(line)
		_ = w.Flush()
	}

	write("220 fake.local ESMTP ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"):
			// 故意不声明 STARTTLS / AUTH，验证客户端只在需要时使用它们。
			write("250-fake.local\r\n250-8BITMIME\r\n250 HELP\r\n")
		case strings.HasPrefix(upper, "HELO"):
			write("250 fake.local\r\n")
		case strings.HasPrefix(upper, "AUTH"):
			s.mu.Lock()
			s.authSeen = true
			s.mu.Unlock()
			write(fmt.Sprintf("%d auth result\r\n", s.opts.authCode))
		case strings.HasPrefix(upper, "MAIL FROM:"):
			from := addressBetweenAngles(strings.TrimSpace(line[len("MAIL FROM:"):]))
			s.mu.Lock()
			s.mailFrom = from
			s.mu.Unlock()
			write("250 2.1.0 Ok\r\n")
		case strings.HasPrefix(upper, "RCPT TO:"):
			to := addressBetweenAngles(strings.TrimSpace(line[len("RCPT TO:"):]))
			s.mu.Lock()
			s.rcpts = append(s.rcpts, to)
			code := s.opts.rcptCode
			s.mu.Unlock()
			if code != 0 {
				write(fmt.Sprintf("%d 5.1.1 mailbox unavailable\r\n", code))
			} else {
				write("250 2.1.5 Ok\r\n")
			}
		case upper == "DATA":
			write("354 end data with <CRLF>.<CRLF>\r\n")
			var data strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				// 还原 SMTP dot-stuffing。
				if strings.HasPrefix(dl, "..") {
					dl = dl[1:]
				}
				data.WriteString(dl)
			}
			s.dataCh <- data.String()
			write("250 2.0.0 Ok: queued\r\n")
		case upper == "QUIT":
			write("221 2.0.0 Bye\r\n")
			return
		case upper == "RSET", upper == "NOOP":
			write("250 2.0.0 Ok\r\n")
		default:
			write("500 5.5.1 command not recognized\r\n")
		}
	}
}

// message 等待并返回假服务器收到的完整邮件原文。
func (s *fakeSMTP) message(t *testing.T) string {
	t.Helper()
	select {
	case raw := <-s.dataCh:
		return raw
	case <-time.After(testTimeout):
		t.Fatal("等待假 SMTP 服务器接收 DATA 超时")
		return ""
	}
}

// snapshot 返回捕获到的 SMTP 信封信息（加锁避免数据竞争）。
func (s *fakeSMTP) snapshot() (mailFrom string, rcpts []string, authSeen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mailFrom, append([]string(nil), s.rcpts...), s.authSeen
}

// addressBetweenAngles 从 "<a@b>" 或 "MAIL FROM:<a@b> BODY=8BITMIME" 中取出地址。
func addressBetweenAngles(s string) string {
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return strings.TrimSpace(s)
}

// newSender 用假服务器地址构造一个 TLS=none、无认证的适配器。
func newSender(t *testing.T, srv *fakeSMTP) *email.Sender {
	t.Helper()
	s, err := email.New(email.Config{
		Host:          "127.0.0.1",
		Port:          srv.port(),
		From:          "unipush@example.com",
		To:            []string{"ops@example.com", "dev@example.com"},
		TLS:           "none",
		SubjectPrefix: "[unipush]",
		Timeout:       testTimeout,
	})
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	return s
}

func TestSendBuildsMultipartEmail(t *testing.T) {
	srv := startFakeSMTP(t, fakeOptions{})
	s := newSender(t, srv)

	if got := s.Name(); got != "email" {
		t.Fatalf("Name() = %q, 期望 email", got)
	}

	title := "构建失败 <deploy>"
	body := "第 3 行 <nil> 未定义\n请检查 & 重试"
	link := "https://ci.example.com/1?a=1&b=2"
	msg := &model.Message{Title: title, Body: body, Level: model.LevelError, Link: link}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	raw := srv.message(t)
	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("解析邮件原文: %v\n原文:\n%s", err, raw)
	}
	hdr := parsed.Header

	if got, want := hdr.Get("To"), "ops@example.com, dev@example.com"; got != want {
		t.Errorf("To = %q, 期望 %q", got, want)
	}
	if got, want := hdr.Get("From"), "unipush@example.com"; got != want {
		t.Errorf("From = %q, 期望 %q", got, want)
	}
	if got, want := hdr.Get("MIME-Version"), "1.0"; got != want {
		t.Errorf("MIME-Version = %q, 期望 %q", got, want)
	}
	if _, err := time.Parse(time.RFC1123Z, hdr.Get("Date")); err != nil {
		t.Errorf("Date %q 不是 RFC1123Z 格式: %v", hdr.Get("Date"), err)
	}

	subject := hdr.Get("Subject")
	if !strings.Contains(subject, "=?utf-8?") {
		t.Errorf("Subject 未被编码: %q", subject)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("解码 Subject: %v", err)
	}
	if want := "[unipush] " + title; decoded != want {
		t.Errorf("Subject 解码 = %q, 期望 %q", decoded, want)
	}

	mediaType, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil {
		t.Fatalf("解析 Content-Type: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Errorf("Content-Type = %q, 期望 multipart/alternative", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("Content-Type 缺少 multipart boundary: %q", hdr.Get("Content-Type"))
	}

	rawBody, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("读取正文: %v", err)
	}
	rawBodyText := string(rawBody)
	if !strings.Contains(rawBodyText, "text/plain") || !strings.Contains(rawBodyText, "text/html") {
		t.Errorf("正文缺少 text/plain 或 text/html 部分:\n%s", rawBodyText)
	}

	parts := map[string]string{}
	mr := multipart.NewReader(bytes.NewReader(rawBody), boundary)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析 multipart: %v", err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("读取 multipart 部分: %v", err)
		}
		parts[p.Header.Get("Content-Type")] = string(data)
	}

	plain, ok := parts["text/plain; charset=utf-8"]
	if !ok {
		t.Fatalf("缺少 text/plain 部分，实际有: %v", keys(parts))
	}
	htmlBody, ok := parts["text/html; charset=utf-8"]
	if !ok {
		t.Fatalf("缺少 text/html 部分，实际有: %v", keys(parts))
	}

	for _, want := range []string{title, "第 3 行 <nil> 未定义", link} {
		if !strings.Contains(plain, want) {
			t.Errorf("text/plain 缺少 %q:\n%s", want, plain)
		}
	}

	escapedTitle := "构建失败 &lt;deploy&gt;"
	checks := []struct {
		name string
		want string
	}{
		{"标题段落", "<p>" + escapedTitle + "</p>"},
		{"正文转义", "第 3 行 &lt;nil&gt; 未定义<br>"},
		{"正文 & 转义", "请检查 &amp; 重试"},
		{"链接", `<a href="https://ci.example.com/1?a=1&amp;b=2">https://ci.example.com/1?a=1&amp;b=2</a>`},
	}
	for _, c := range checks {
		if !strings.Contains(htmlBody, c.want) {
			t.Errorf("text/html 缺少%s %q:\n%s", c.name, c.want, htmlBody)
		}
	}
	if strings.Contains(htmlBody, "<deploy>") || strings.Contains(htmlBody, "<nil>") {
		t.Errorf("text/html 中的标题或正文没有被转义:\n%s", htmlBody)
	}

	mailFrom, rcpts, authSeen := srv.snapshot()
	if mailFrom != "unipush@example.com" {
		t.Errorf("MAIL FROM = %q, 期望 unipush@example.com", mailFrom)
	}
	if got, want := strings.Join(rcpts, ","), "ops@example.com,dev@example.com"; got != want {
		t.Errorf("RCPT TO = %q, 期望 %q", got, want)
	}
	if authSeen {
		t.Error("Username 为空时不应发送 AUTH")
	}
}

func TestSendRCPTRejectedIsPermanent(t *testing.T) {
	srv := startFakeSMTP(t, fakeOptions{rcptCode: 550})
	s := newSender(t, srv)

	err := s.Send(context.Background(), &model.Message{Title: "标题", Body: "正文"})
	if err == nil {
		t.Fatal("RCPT 550 时 Send 应该返回错误")
	}
	if !channel.IsPermanent(err) {
		t.Fatalf("RCPT 550 应归类为不可重试错误，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("错误信息应包含 550，实际: %v", err)
	}
}

func TestSendConnectionRefusedIsRetryable(t *testing.T) {
	// 先占一个端口再关闭，得到一个必然拒绝连接的端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("关闭临时监听: %v", err)
	}

	s, err := email.New(email.Config{
		Host:    "127.0.0.1",
		Port:    port,
		From:    "unipush@example.com",
		To:      []string{"ops@example.com"},
		TLS:     "none",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}

	err = s.Send(context.Background(), &model.Message{Title: "标题", Body: "正文"})
	if err == nil {
		t.Fatal("连接被拒绝时 Send 应该返回错误")
	}
	if channel.IsPermanent(err) {
		t.Fatalf("连接错误应归类为可重试，实际: %v", err)
	}
}

func TestSendDefaultsToSTARTTLSWhenSupported(t *testing.T) {
	// 假服务器不声明 STARTTLS：默认的 starttls 模式应退化为明文继续发送。
	srv := startFakeSMTP(t, fakeOptions{})
	s, err := email.New(email.Config{
		Host:    "127.0.0.1",
		Port:    srv.port(),
		From:    "unipush@example.com",
		To:      []string{"ops@example.com"},
		Timeout: testTimeout,
	})
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}

	if err := s.Send(context.Background(), &model.Message{Title: "build failed", Body: "see log"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	parsed, err := mail.ReadMessage(strings.NewReader(srv.message(t)))
	if err != nil {
		t.Fatalf("解析邮件原文: %v", err)
	}
	// 前缀为空时 Subject 直接用标题；纯 ASCII 不应被编码。
	if got, want := parsed.Header.Get("Subject"), "build failed"; got != want {
		t.Errorf("Subject = %q, 期望 %q", got, want)
	}
	if _, rcpts, authSeen := srv.snapshot(); len(rcpts) != 1 || authSeen {
		t.Errorf("RCPT 数量 = %d, AUTH = %v", len(rcpts), authSeen)
	}
}

func TestSendAuthenticatesWhenUsernameSet(t *testing.T) {
	srv := startFakeSMTP(t, fakeOptions{authCode: 235})
	s, err := email.New(email.Config{
		Host:     "127.0.0.1",
		Port:     srv.port(),
		Username: "unipush",
		Password: "secret",
		From:     "unipush@example.com",
		To:       []string{"ops@example.com"},
		TLS:      "none",
		Timeout:  testTimeout,
	})
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}

	if err := s.Send(context.Background(), &model.Message{Title: "标题", Body: "正文"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, _, authSeen := srv.snapshot(); !authSeen {
		t.Error("配置了 Username 时应发送 AUTH")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	base := func() email.Config {
		return email.Config{
			Host: "smtp.example.com",
			From: "unipush@example.com",
			To:   []string{"ops@example.com"},
			TLS:  "none",
		}
	}

	tests := []struct {
		name string
		edit func(*email.Config)
	}{
		{"缺少 host", func(c *email.Config) { c.Host = "" }},
		{"缺少 from", func(c *email.Config) { c.From = "" }},
		{"没有收件人", func(c *email.Config) { c.To = nil }},
		{"收件人全是空串", func(c *email.Config) { c.To = []string{"", "  "} }},
		{"非法 tls", func(c *email.Config) { c.TLS = "tls" }},
		{"端口越界", func(c *email.Config) { c.Port = 70000 }},
		{"host 含换行", func(c *email.Config) { c.Host = "smtp.example.com\r\nX: y" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.edit(&cfg)
			if _, err := email.New(cfg); err == nil {
				t.Fatal("期望 New 返回错误，实际为 nil")
			}
		})
	}

	if _, err := email.New(base()); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
}

// keys 返回 map 的键，用于失败信息。
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
