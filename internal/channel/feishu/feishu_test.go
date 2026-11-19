package feishu_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/feishu"
	"github.com/ziwindlu/unipush/internal/model"
)

const testSecret = "SEC-feishu-secret"

// newTestSender 创建指向 httptest 服务的适配器。
func newTestSender(t *testing.T, cfg feishu.Config) *feishu.Sender {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	s, err := feishu.New(cfg)
	if err != nil {
		t.Fatalf("feishu.New: %v", err)
	}
	return s
}

// textPayload 是 text 形态请求体的解析结果。
type textPayload struct {
	MsgType string `json:"msg_type"`
	Content struct {
		Text string `json:"text"`
	} `json:"content"`
	Timestamp string `json:"timestamp"`
	Sign      string `json:"sign"`
}

// cardPayload 是 card 形态请求体的解析结果。
type cardPayload struct {
	MsgType string `json:"msg_type"`
	Card    struct {
		Header struct {
			Title struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"title"`
			Template string `json:"template"`
		} `json:"header"`
		Elements []struct {
			Tag  string `json:"tag"`
			Text struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"text"`
			Actions []struct {
				Tag  string `json:"tag"`
				URL  string `json:"url"`
				Text struct {
					Content string `json:"content"`
				} `json:"text"`
			} `json:"actions"`
		} `json:"elements"`
	} `json:"card"`
}

func TestName(t *testing.T) {
	s := newTestSender(t, feishu.Config{Webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/x"})
	if got := s.Name(); got != "feishu" {
		t.Fatalf("Name() = %q, 期望 feishu", got)
	}
}

func TestSendTextPayload(t *testing.T) {
	tests := []struct {
		name string
		msg  *model.Message
		want string
	}{
		{
			name: "标题正文链接各占一行",
			msg:  &model.Message{Title: "磁盘告警", Body: "剩余 3%", Link: "https://example.com/alert/1"},
			want: "磁盘告警\n剩余 3%\nhttps://example.com/alert/1",
		},
		{
			name: "空正文行省略",
			msg:  &model.Message{Title: "磁盘告警", Body: "   ", Link: "https://example.com/alert/1"},
			want: "磁盘告警\nhttps://example.com/alert/1",
		},
		{
			name: "只有正文",
			msg:  &model.Message{Body: "纯文本"},
			want: "纯文本",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got textPayload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("解析请求体: %v", err)
				}
				io.WriteString(w, `{"code":0,"msg":"success"}`)
			}))
			defer srv.Close()

			s := newTestSender(t, feishu.Config{Webhook: srv.URL})
			if err := s.Send(context.Background(), tc.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got.MsgType != "text" {
				t.Errorf("msg_type = %q, 期望 text", got.MsgType)
			}
			if got.Content.Text != tc.want {
				t.Errorf("content.text = %q, 期望 %q", got.Content.Text, tc.want)
			}
		})
	}
}

func TestSendCardPayload(t *testing.T) {
	tests := []struct {
		level        string
		wantTemplate string
	}{
		{model.LevelInfo, "blue"},
		{model.LevelWarn, "orange"},
		{model.LevelError, "red"},
		{"", "blue"},
		{"unknown", "blue"},
	}

	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			var got cardPayload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("解析请求体: %v", err)
				}
				io.WriteString(w, `{"code":0,"msg":"success"}`)
			}))
			defer srv.Close()

			s := newTestSender(t, feishu.Config{Webhook: srv.URL, MsgType: "card"})
			msg := &model.Message{
				Title: "磁盘告警",
				Body:  "剩余 3%",
				Level: tc.level,
				Link:  "https://example.com/alert/1",
			}
			if err := s.Send(context.Background(), msg); err != nil {
				t.Fatalf("Send: %v", err)
			}

			if got.MsgType != "interactive" {
				t.Errorf("msg_type = %q, 期望 interactive", got.MsgType)
			}
			if got.Card.Header.Template != tc.wantTemplate {
				t.Errorf("template = %q, 期望 %q", got.Card.Header.Template, tc.wantTemplate)
			}
			if got.Card.Header.Title.Tag != "plain_text" {
				t.Errorf("header.title.tag = %q, 期望 plain_text", got.Card.Header.Title.Tag)
			}
			if got.Card.Header.Title.Content != "磁盘告警" {
				t.Errorf("header.title.content = %q, 期望 磁盘告警", got.Card.Header.Title.Content)
			}

			// 第一个 element 是正文 div，正文用 lark_md。
			if len(got.Card.Elements) != 2 {
				t.Fatalf("elements 数量 = %d, 期望 2（正文 + 链接）", len(got.Card.Elements))
			}
			body := got.Card.Elements[0]
			if body.Tag != "div" || body.Text.Tag != "lark_md" || body.Text.Content != "剩余 3%" {
				t.Errorf("正文元素不符合预期: %+v", body)
			}

			// 有 link 时追加一个带 url 的 action 元素。
			action := got.Card.Elements[1]
			if action.Tag != "action" || len(action.Actions) != 1 {
				t.Fatalf("链接元素不符合预期: %+v", action)
			}
			if action.Actions[0].URL != "https://example.com/alert/1" {
				t.Errorf("按钮 url = %q, 期望 https://example.com/alert/1", action.Actions[0].URL)
			}
		})
	}
}

func TestSendCardWithoutLinkHasSingleElement(t *testing.T) {
	var got cardPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		io.WriteString(w, `{"code":0,"msg":"success"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, feishu.Config{Webhook: srv.URL, MsgType: "card"})
	if err := s.Send(context.Background(), &model.Message{Body: "only body"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got.Card.Elements) != 1 {
		t.Fatalf("elements 数量 = %d, 期望 1（无链接）", len(got.Card.Elements))
	}
}

func TestSendSignsRequest(t *testing.T) {
	const timestamp = "1700000000"
	fixed := time.Unix(1700000000, 0)

	var got textPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		io.WriteString(w, `{"code":0,"msg":"success"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, feishu.Config{
		Webhook: srv.URL,
		Secret:  testSecret,
		Now:     func() time.Time { return fixed },
	})
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.Timestamp != timestamp {
		t.Errorf("timestamp = %q, 期望 %q（Unix 秒）", got.Timestamp, timestamp)
	}

	// 用请求体里的 timestamp 和服务端同一 secret 独立重算，锁定加签算法：
	// key = timestamp+"\n"+secret，msg = ""，HMAC-SHA256 后 base64。
	mac := hmac.New(sha256.New, []byte(got.Timestamp+"\n"+testSecret))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got.Sign != want {
		t.Errorf("sign = %q, 期望 %q", got.Sign, want)
	}
}

func TestSendWithoutSecretDoesNotSign(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		io.WriteString(w, `{"code":0,"msg":"success"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, feishu.Config{Webhook: srv.URL})
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := body["sign"]; ok {
		t.Error("未配置 secret 时不应包含 sign")
	}
	if _, ok := body["timestamp"]; ok {
		t.Error("未配置 secret 时不应包含 timestamp")
	}
}

func TestSendSetsJSONContentType(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		io.WriteString(w, `{"code":0,"msg":"success"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, feishu.Config{Webhook: srv.URL})
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, 期望包含 application/json", gotContentType)
	}
}

func TestSendSuccessResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"新版本 code", `{"code":0,"msg":"success","data":{}}`},
		{"老版本 StatusCode", `{"StatusCode":0,"StatusMessage":"success"}`},
		{"仅 code 字段", `{"code":0}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			s := newTestSender(t, feishu.Config{Webhook: srv.URL})
			if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
				t.Fatalf("Send: %v, 期望成功", err)
			}
		})
	}
}

func TestSendErrorCodeIsPermanent(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"签名校验失败 19021", `{"code":19021,"msg":"sign match fail or timestamp is not within one hour"}`},
		{"其它业务错误 19002", `{"code":19002,"msg":"param invalid"}`},
		{"老版本 StatusCode 非 0", `{"StatusCode":19021,"StatusMessage":"sign match fail"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			err := newTestSender(t, feishu.Config{Webhook: srv.URL}).
				Send(context.Background(), &model.Message{Body: "x"})
			if err == nil {
				t.Fatal("期望返回错误")
			}
			if !channel.IsPermanent(err) {
				t.Errorf("业务错误码应不可重试: %v", err)
			}
		})
	}
}

func TestSendRateLimitCodeIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":9499,"msg":"too many requests"}`)
	}))
	defer srv.Close()

	err := newTestSender(t, feishu.Config{Webhook: srv.URL}).
		Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("限流错误码应可重试: %v", err)
	}
}

func TestSendHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		status    int
		permanent bool
	}{
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusRequestTimeout, false},
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusNotFound, true},
	}

	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"code":123,"msg":"oops"}`)
			}))
			defer srv.Close()

			err := newTestSender(t, feishu.Config{Webhook: srv.URL}).
				Send(context.Background(), &model.Message{Body: "x"})
			if err == nil {
				t.Fatal("期望返回错误")
			}
			if got := channel.IsPermanent(err); got != tc.permanent {
				t.Errorf("HTTP %d IsPermanent = %v, 期望 %v（err=%v）", tc.status, got, tc.permanent, err)
			}
		})
	}
}

func TestSendNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	webhook := srv.URL + "/open-apis/bot/v2/hook/x"
	srv.Close() // 立即关闭，制造连接失败

	err := newTestSender(t, feishu.Config{Webhook: webhook}).
		Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("网络错误应可重试: %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     feishu.Config
		wantErr bool
	}{
		{"空 webhook", feishu.Config{Webhook: ""}, true},
		{"非 URL", feishu.Config{Webhook: "not-a-url"}, true},
		{"非 http scheme", feishu.Config{Webhook: "ftp://example.com/hook"}, true},
		{"缺少 host", feishu.Config{Webhook: "https:///hook"}, true},
		{"合法 https", feishu.Config{Webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/x"}, false},
		{"合法 http", feishu.Config{Webhook: "http://127.0.0.1:8080/hook"}, false},
		{"msg_type 为空按 text", feishu.Config{Webhook: "https://example.com/hook", MsgType: ""}, false},
		{"msg_type 大小写归一", feishu.Config{Webhook: "https://example.com/hook", MsgType: " Card "}, false},
		{"非法 msg_type", feishu.Config{Webhook: "https://example.com/hook", MsgType: "markdown"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := feishu.New(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("期望返回错误")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不期望错误: %v", err)
			}
		})
	}
}

func TestMsgTypeDefaultsToText(t *testing.T) {
	var got textPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		io.WriteString(w, `{"code":0,"msg":"success"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, feishu.Config{Webhook: srv.URL})
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.MsgType != "text" {
		t.Errorf("msg_type = %q, 期望 text", got.MsgType)
	}
}
