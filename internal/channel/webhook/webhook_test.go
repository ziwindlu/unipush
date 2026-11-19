package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/webhook"
	"github.com/ziwindlu/unipush/internal/model"
)

func newTestSender(t *testing.T, cfg webhook.Config) *webhook.Sender {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	s, err := webhook.New(cfg)
	if err != nil {
		t.Fatalf("webhook.New: %v", err)
	}
	return s
}

func TestRenderTemplateAndHeaders(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotToken       string
		gotBody        string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("X-Token")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	s := newTestSender(t, webhook.Config{
		URL:          srv.URL,
		Method:       http.MethodPost,
		Headers:      map[string]string{"x-token": "s3cret"},
		ContentType:  "text/plain; charset=utf-8",
		BodyTemplate: `{"text":"{{.Title}}: {{.Body}}"}`,
	})
	if got := s.Name(); got != "webhook" {
		t.Fatalf("Name() = %q, 期望 webhook", got)
	}

	msg := &model.Message{
		Title:  "磁盘告警",
		Body:   "剩余 3%",
		Level:  model.LevelError,
		Source: "node-1",
	}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, 期望 POST", gotMethod)
	}
	if want := `{"text":"磁盘告警: 剩余 3%"}`; gotBody != want {
		t.Errorf("请求体 = %q, 期望 %q", gotBody, want)
	}
	if gotContentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, 期望配置值", gotContentType)
	}
	if gotToken != "s3cret" {
		t.Errorf("X-Token = %q, 期望 s3cret", gotToken)
	}
}

func TestHeadersOverrideContentType(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	s := newTestSender(t, webhook.Config{
		URL:         srv.URL,
		ContentType: "application/json; charset=utf-8",
		// http.Header.Set 会规范化大小写，因此这里用小写也能覆盖默认值。
		Headers: map[string]string{"content-type": "application/vnd.custom+json"},
	})
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotContentType != "application/vnd.custom+json" {
		t.Errorf("Content-Type = %q, 期望被 Headers 覆盖", gotContentType)
	}
}

func TestTemplateDataExposesJSONAndExtra(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	s := newTestSender(t, webhook.Config{
		URL:          srv.URL,
		BodyTemplate: `{"title":"{{.Title}}","host":"{{.Extra.host}}","raw":{{.JSON}}}`,
	})
	msg := &model.Message{
		Title: "部署完成",
		Body:  "v1.2.3",
		Extra: map[string]any{"host": "db-1"},
	}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got struct {
		Title string        `json:"title"`
		Host  string        `json:"host"`
		Raw   model.Message `json:"raw"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("渲染结果不是合法 JSON: %v (body=%s)", err, gotBody)
	}
	if got.Title != "部署完成" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Host != "db-1" {
		t.Errorf("host = %q, 期望来自 Extra", got.Host)
	}
	if got.Raw.Title != msg.Title || got.Raw.Body != msg.Body {
		t.Errorf("raw 字段不是消息的 JSON: %+v", got.Raw)
	}
}

func TestDefaultBodyIsMessageJSON(t *testing.T) {
	var (
		gotBody        []byte
		gotContentType string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotContentType = r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	msg := &model.Message{
		ID:        "msg_01",
		Title:     "周报",
		Body:      "本周完成 3 项",
		Level:     model.LevelWarn,
		Source:    "cron",
		Targets:   []string{"ops", "dev"},
		DedupeKey: "weekly-2024w01",
		Link:      "https://example.com/report/1",
		Extra:     map[string]any{"host": "db-1", "count": float64(3)},
		CreatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	want, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	s := newTestSender(t, webhook.Config{URL: srv.URL})
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if string(gotBody) != string(want) {
		t.Fatalf("请求体 = %s, 期望 %s", gotBody, want)
	}
	if gotContentType != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, 期望默认 application/json", gotContentType)
	}

	var back model.Message
	if err := json.Unmarshal(gotBody, &back); err != nil {
		t.Fatalf("请求体无法反序列化为 model.Message: %v", err)
	}
	created := back.CreatedAt
	back.CreatedAt = time.Time{}
	wantMsg := *msg
	wantMsg.CreatedAt = time.Time{}
	if !reflect.DeepEqual(back, wantMsg) {
		t.Errorf("反序列化结果不一致:\n got = %+v\nwant = %+v", back, wantMsg)
	}
	if !created.Equal(msg.CreatedAt) {
		t.Errorf("CreatedAt = %v, 期望 %v", created, msg.CreatedAt)
	}
}

func TestNewRejectsInvalidTemplate(t *testing.T) {
	if _, err := webhook.New(webhook.Config{
		URL:          "https://example.com/hook",
		BodyTemplate: `{"text":"{{.Title"}`,
	}); err == nil {
		t.Fatal("模板语法错误时 New 应返回 error")
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  webhook.Config
	}{
		{"空 URL", webhook.Config{}},
		{"非 http(s) URL", webhook.Config{URL: "ftp://example.com/hook"}},
		{"缺少 host", webhook.Config{URL: "http:///hook"}},
		{"非法方法", webhook.Config{URL: "https://example.com/hook", Method: "BAD METHOD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := webhook.New(tc.cfg); err == nil {
				t.Fatal("期望 New 返回 error")
			}
		})
	}

	// 最小合法配置：URL 必填，其余取默认值。
	s, err := webhook.New(webhook.Config{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("合法配置 New 失败: %v", err)
	}
	if s.Name() != "webhook" {
		t.Errorf("Name() = %q, 期望 webhook", s.Name())
	}
}

func TestSendStatusClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantNil  bool
		wantPerm bool
	}{
		{"200 成功", http.StatusOK, true, false},
		{"204 成功", http.StatusNoContent, true, false},
		{"400 参数错误不可重试", http.StatusBadRequest, false, true},
		{"404 地址错误不可重试", http.StatusNotFound, false, true},
		{"408 超时可重试", http.StatusRequestTimeout, false, false},
		{"429 限流可重试", http.StatusTooManyRequests, false, false},
		{"500 服务端错误可重试", http.StatusInternalServerError, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":"boom"}`)
			}))
			defer srv.Close()

			s := newTestSender(t, webhook.Config{URL: srv.URL})
			err := s.Send(context.Background(), &model.Message{Title: "t", Body: "b"})
			if tc.wantNil {
				if err != nil {
					t.Fatalf("期望成功，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望返回错误")
			}
			if got := channel.IsPermanent(err); got != tc.wantPerm {
				t.Errorf("IsPermanent = %v, 期望 %v (err=%v)", got, tc.wantPerm, err)
			}
			// 错误信息要带上状态码和响应体片段，方便排障。
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("错误信息缺少状态码 %d: %v", tc.status, err)
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Errorf("错误信息缺少响应体片段: %v", err)
			}
		})
	}
}

func TestCustomMethod(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
	}))
	defer srv.Close()

	// 小写方法会在 New 里归一化为大写。
	s := newTestSender(t, webhook.Config{URL: srv.URL, Method: "put"})
	if err := s.Send(context.Background(), &model.Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, 期望 PUT", gotMethod)
	}
}

func TestNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := srv.URL
	srv.Close() // 立即关闭，制造连接失败

	err := newTestSender(t, webhook.Config{URL: target}).Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("网络错误应可重试: %v", err)
	}
}
