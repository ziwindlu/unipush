package wecom_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/wecom"
	"github.com/ziwindlu/unipush/internal/model"
)

// newTestSender 创建一个指向测试服务的适配器。
func newTestSender(t *testing.T, webhook string, msgType string) *wecom.Sender {
	t.Helper()
	s, err := wecom.New(wecom.Config{
		Webhook: webhook,
		MsgType: msgType,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("wecom.New: %v", err)
	}
	return s
}

// responseRecorder 记录一次请求的关键信息。
type responseRecorder struct {
	method      string
	contentType string
	rawBody     string
	msgType     string
	markdown    string
	text        string
}

// newRecorder 启动一个返回固定响应的测试服务，并记录收到的请求。
func newRecorder(t *testing.T, status int, respBody string) (*httptest.Server, *responseRecorder) {
	t.Helper()
	rec := &responseRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.contentType = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体: %v", err)
		}
		rec.rawBody = string(raw)

		var body struct {
			MsgType  string `json:"msgtype"`
			Markdown *struct {
				Content string `json:"content"`
			} `json:"markdown"`
			Text *struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		rec.msgType = body.MsgType
		if body.Markdown != nil {
			rec.markdown = body.Markdown.Content
		}
		if body.Text != nil {
			rec.text = body.Text.Content
		}

		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestSendMarkdownRequestBody(t *testing.T) {
	srv, rec := newRecorder(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)

	s := newTestSender(t, srv.URL+"/cgi-bin/webhook/send?key=test-key", "")
	err := s.Send(context.Background(), &model.Message{
		Title: "磁盘告警",
		Body:  "剩余 3%",
		Link:  "https://example.com/alert/1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if rec.method != http.MethodPost {
		t.Errorf("method = %s, 期望 POST", rec.method)
	}
	if !strings.HasPrefix(rec.contentType, "application/json") {
		t.Errorf("Content-Type = %q, 期望 application/json", rec.contentType)
	}
	if rec.msgType != "markdown" {
		t.Errorf("msgtype = %q, 期望 markdown", rec.msgType)
	}
	want := "**磁盘告警**\n剩余 3%\n[详情](https://example.com/alert/1)"
	if rec.markdown != want {
		t.Errorf("markdown content = %q, 期望 %q", rec.markdown, want)
	}
	if rec.text != "" || strings.Contains(rec.rawBody, `"text"`) {
		t.Errorf("markdown 形态不应携带 text 字段，请求体: %s", rec.rawBody)
	}
}

func TestSendMarkdownOmitsEmptyParts(t *testing.T) {
	cases := []struct {
		name string
		msg  *model.Message
		want string
	}{
		{
			name: "仅标题",
			msg:  &model.Message{Title: "只有标题"},
			want: "**只有标题**",
		},
		{
			name: "正文与链接",
			msg:  &model.Message{Body: "正文", Link: "https://example.com/x"},
			want: "正文\n[详情](https://example.com/x)",
		},
		{
			name: "全空",
			msg:  &model.Message{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := newRecorder(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
			if err := newTestSender(t, srv.URL, "markdown").Send(context.Background(), tc.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if rec.markdown != tc.want {
				t.Errorf("markdown content = %q, 期望 %q", rec.markdown, tc.want)
			}
		})
	}
}

func TestSendTextRequestBody(t *testing.T) {
	srv, rec := newRecorder(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)

	// text 形态只拼接标题与正文，链接不进正文（需求约定的形态）。
	err := newTestSender(t, srv.URL, "text").Send(context.Background(), &model.Message{
		Title: "构建失败",
		Body:  "详见日志",
		Link:  "https://ci.example.com/1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if rec.msgType != "text" {
		t.Errorf("msgtype = %q, 期望 text", rec.msgType)
	}
	if want := "构建失败\n详见日志"; rec.text != want {
		t.Errorf("text content = %q, 期望 %q", rec.text, want)
	}
	if rec.markdown != "" || strings.Contains(rec.rawBody, `"markdown"`) {
		t.Errorf("text 形态不应携带 markdown 字段，请求体: %s", rec.rawBody)
	}
}

func TestSendTextOmitsEmptyTitle(t *testing.T) {
	srv, rec := newRecorder(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
	if err := newTestSender(t, srv.URL, "text").Send(context.Background(), &model.Message{Body: "只有正文"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.text != "只有正文" {
		t.Errorf("text content = %q, 期望 %q", rec.text, "只有正文")
	}
}

func TestSendTruncatesLongMarkdownContentOnUTF8Boundary(t *testing.T) {
	body := strings.Repeat("汉", 5000) // 15000 字节，远超 4096
	srv, rec := newRecorder(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)

	if err := newTestSender(t, srv.URL, "markdown").Send(context.Background(), &model.Message{Body: body}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := rec.markdown
	if len(got) > 4096 {
		t.Errorf("截断后长度 = %d 字节，超过上限 4096", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("截断后不是合法 UTF-8，说明切出了半个汉字: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("截断后出现替换符，说明切出了半个汉字: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应以省略号结尾，实际结尾: %q", tail(got))
	}
	// 每 3 个字节一个汉字：预留省略号后最多保留 1364 个汉字。
	want := strings.Repeat("汉", 1364) + "…"
	if got != want {
		t.Errorf("截断结果 = %q（%d 字节），期望 %q（%d 字节）",
			tail(got), len(got), tail(want), len(want))
	}
}

func TestSendDoesNotTruncateShortContent(t *testing.T) {
	srv, rec := newRecorder(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
	// 刚好 4096 字节：标题加粗后内容仍不超限，不应截断。
	body := strings.Repeat("a", 4096-len("**T**")-1)
	if err := newTestSender(t, srv.URL, "markdown").Send(context.Background(), &model.Message{
		Title: "T",
		Body:  body,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.HasSuffix(rec.markdown, "…") {
		t.Error("未超限的内容不应被截断")
	}
	if len(rec.markdown) != 4096 {
		t.Errorf("content 长度 = %d 字节，期望 4096", len(rec.markdown))
	}
}

func TestSendRateLimitErrCodesAreRetryable(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"接口调用超过限制", 45009},
		{"客服接口超过限制", 45047},
		{"api 调用太频繁", 45011},
		{"系统繁忙", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"errcode":` + itoa(tc.code) + `,"errmsg":"rate limited"}`
			srv, _ := newRecorder(t, http.StatusOK, body)
			err := newTestSender(t, srv.URL, "").Send(context.Background(), &model.Message{Body: "x"})
			if err == nil {
				t.Fatal("期望返回错误")
			}
			if channel.IsPermanent(err) {
				t.Errorf("errcode=%d 属于限流，应可重试: %v", tc.code, err)
			}
		})
	}
}

func TestSendOtherErrCodeIsPermanent(t *testing.T) {
	srv, _ := newRecorder(t, http.StatusOK, `{"errcode":93000,"errmsg":"invalid webhook url"}`)
	err := newTestSender(t, srv.URL, "").Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !channel.IsPermanent(err) {
		t.Errorf("errcode=93000 应不可重试: %v", err)
	}
}

func TestSendHTTPStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"429 限流", http.StatusTooManyRequests, false},
		{"408 超时", http.StatusRequestTimeout, false},
		{"500 服务端错误", http.StatusInternalServerError, false},
		{"502 网关错误", http.StatusBadGateway, false},
		{"400 参数错误", http.StatusBadRequest, true},
		{"403 鉴权失败", http.StatusForbidden, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newRecorder(t, tc.status, `{"errcode":0,"errmsg":"ok"}`)
			err := newTestSender(t, srv.URL, "").Send(context.Background(), &model.Message{Body: "x"})
			if err == nil {
				t.Fatal("期望返回错误")
			}
			if got := channel.IsPermanent(err); got != tc.permanent {
				t.Errorf("HTTP %d: IsPermanent = %v, 期望 %v（err: %v）",
					tc.status, got, tc.permanent, err)
			}
		})
	}
}

func TestSendNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	webhook := srv.URL + "/send"
	srv.Close() // 立即关闭，制造连接失败

	err := newTestSender(t, webhook, "").Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("网络错误应可重试: %v", err)
	}
}

func TestSendInvalidJSONResponseIsRetryable(t *testing.T) {
	srv, _ := newRecorder(t, http.StatusOK, `<html>bad gateway</html>`)
	err := newTestSender(t, srv.URL, "").Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("无法解析的响应应可重试: %v", err)
	}
}

func TestName(t *testing.T) {
	if got := newTestSender(t, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x", "").Name(); got != "wecom" {
		t.Errorf("Name() = %q, 期望 wecom", got)
	}
}

func TestNewDefaultsAndValidation(t *testing.T) {
	valid := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x"

	// 合法配置，空 msgtype 按 markdown。
	if _, err := wecom.New(wecom.Config{Webhook: valid}); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}

	badURLs := []string{
		"",
		"   ",
		"not-a-url",
		"://missing-scheme",
		"ftp://example.com/hook",
		"https:///no-host",
		"qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x",
	}
	for _, webhook := range badURLs {
		if _, err := wecom.New(wecom.Config{Webhook: webhook}); err == nil {
			t.Errorf("webhook %q 应被拒绝", webhook)
		}
	}

	for _, msgType := range []string{"image", "news", "markdownx"} {
		if _, err := wecom.New(wecom.Config{Webhook: valid, MsgType: msgType}); err == nil {
			t.Errorf("msgtype %q 应被拒绝", msgType)
		}
	}

	for _, msgType := range []string{"", "markdown", "text", " Markdown "} {
		if _, err := wecom.New(wecom.Config{Webhook: valid, MsgType: msgType}); err != nil {
			t.Errorf("msgtype %q 应被接受: %v", msgType, err)
		}
	}
}

// itoa 避免测试文件为了一个整数引入 strconv，保持断言简单。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// tail 返回字符串结尾若干字节，便于在失败信息里展示截断结果。
func tail(s string) string {
	const n = 24
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
