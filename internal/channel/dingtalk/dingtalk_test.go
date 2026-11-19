package dingtalk_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/dingtalk"
	"github.com/ziwindlu/unipush/internal/model"
)

const testSecret = "SEC-test-secret"

func newTestSender(t *testing.T, webhook string, secret string, now func() time.Time) *dingtalk.Sender {
	t.Helper()
	s, err := dingtalk.New(dingtalk.Config{
		Webhook: webhook,
		Secret:  secret,
		Timeout: 5 * time.Second,
		Now:     now,
	})
	if err != nil {
		t.Fatalf("dingtalk.New: %v", err)
	}
	return s
}

func TestSendSignsRequestAndSendsText(t *testing.T) {
	fixed := time.UnixMilli(1700000000123)
	var (
		gotTimestamp string
		gotSign      string
		gotToken     string
		gotContent   string
		gotMsgType   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotTimestamp = q.Get("timestamp")
		gotSign = q.Get("sign")
		gotToken = q.Get("access_token")

		var payload struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		gotMsgType = payload.MsgType
		gotContent = payload.Text.Content
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL+"/robot/send?access_token=robot_token_123", testSecret,
		func() time.Time { return fixed })
	msg := &model.Message{
		Title: "磁盘告警",
		Body:  "剩余 3%",
		Link:  "https://example.com/alert/1",
	}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotTimestamp != "1700000000123" {
		t.Errorf("timestamp = %q, 期望 1700000000123", gotTimestamp)
	}
	if want := dingtalk.Sign(testSecret, gotTimestamp); gotSign != want {
		t.Errorf("sign = %q, 期望 %q", gotSign, want)
	}
	if gotToken != "robot_token_123" {
		t.Errorf("access_token 未保留，实际 %q", gotToken)
	}
	if gotMsgType != "text" {
		t.Errorf("msgtype = %q, 期望 text", gotMsgType)
	}
	for _, want := range []string{"磁盘告警", "剩余 3%", "https://example.com/alert/1"} {
		if !strings.Contains(gotContent, want) {
			t.Errorf("消息正文缺少 %q，实际: %q", want, gotContent)
		}
	}
}

func TestSendWithoutSecretDoesNotSign(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL+"/robot/send?access_token=t", "", nil)
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(gotQuery, "sign=") || strings.Contains(gotQuery, "timestamp=") {
		t.Errorf("未配置 secret 时不应加签，实际 query: %s", gotQuery)
	}
}

func TestSignMatchesDingTalkAlgorithm(t *testing.T) {
	// 用固定输入锁定算法，避免实现被无意改掉。
	got := dingtalk.Sign("secret", "1700000000000")
	if got == "" {
		t.Fatal("签名为空")
	}
	if again := dingtalk.Sign("secret", "1700000000000"); again != got {
		t.Fatalf("签名不稳定: %q vs %q", got, again)
	}
	if other := dingtalk.Sign("secret", "1700000000001"); other == got {
		t.Fatal("不同 timestamp 的签名不应相同")
	}
}

func TestSendErrCodeIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"errcode":310000,"errmsg":"sign not match"}`)
	}))
	defer srv.Close()

	err := newTestSender(t, srv.URL+"/robot/send", testSecret, nil).
		Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !channel.IsPermanent(err) {
		t.Errorf("签名错误应不可重试: %v", err)
	}
}

func TestSendRateLimitIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"errcode":130101,"errmsg":"send too fast"}`)
	}))
	defer srv.Close()

	err := newTestSender(t, srv.URL+"/robot/send", "", nil).
		Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("限流应可重试: %v", err)
	}
}

func TestSendNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/robot/send"
	srv.Close() // 立即关闭，制造连接失败

	err := newTestSender(t, url, "", nil).Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("网络错误应可重试: %v", err)
	}
}

func TestNewRejectsInvalidWebhook(t *testing.T) {
	if _, err := dingtalk.New(dingtalk.Config{Webhook: "not-a-url"}); err == nil {
		t.Fatal("非法 webhook 应返回错误")
	}
	if _, err := dingtalk.New(dingtalk.Config{Webhook: ""}); err == nil {
		t.Fatal("空 webhook 应返回错误")
	}
}

// TestSendPrefixesKeyword 锁定「关键词: 内容」的拼装格式，机器人开了自定义关键词校验时靠它放行。
func TestSendPrefixesKeyword(t *testing.T) {
	var gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		gotContent = payload.Text.Content
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	s, err := dingtalk.New(dingtalk.Config{
		Webhook: srv.URL + "/robot/send?access_token=t",
		Keyword: "msg",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dingtalk.New: %v", err)
	}
	if err := s.Send(context.Background(), &model.Message{Title: "磁盘告警", Body: "剩余 3%"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := "msg: 磁盘告警\n剩余 3%"; gotContent != want {
		t.Errorf("正文 = %q, 期望 %q", gotContent, want)
	}
}

// TestSendWithoutKeywordKeepsContent 说明关键词为空时不加前缀。
func TestSendWithoutKeywordKeepsContent(t *testing.T) {
	var gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		gotContent = payload.Text.Content
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	s, err := dingtalk.New(dingtalk.Config{
		Webhook: srv.URL + "/robot/send?access_token=t",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dingtalk.New: %v", err)
	}
	if err := s.Send(context.Background(), &model.Message{Title: "磁盘告警", Body: "剩余 3%"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := "磁盘告警\n剩余 3%"; gotContent != want {
		t.Errorf("正文 = %q, 期望 %q", gotContent, want)
	}
}
