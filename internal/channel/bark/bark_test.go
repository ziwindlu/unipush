package bark_test

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
	"github.com/ziwindlu/unipush/internal/channel/bark"
	"github.com/ziwindlu/unipush/internal/model"
)

func newTestSender(t *testing.T, serverURL string) *bark.Sender {
	t.Helper()
	s, err := bark.New(bark.Config{
		ServerURL: serverURL,
		DeviceKey: "device-key-001",
		Group:     "unipush",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("bark.New: %v", err)
	}
	return s
}

func TestSendBuildsPushRequest(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, 期望 application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":200,"message":"success"}`)
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	err := s.Send(context.Background(), &model.Message{
		Title: "构建失败",
		Body:  "详见日志",
		Level: model.LevelError,
		Link:  "https://ci.example.com/1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, 期望 POST", gotMethod)
	}
	if gotPath != "/push" {
		t.Errorf("path = %s, 期望 /push", gotPath)
	}
	want := map[string]string{
		"device_key": "device-key-001",
		"title":      "构建失败",
		"body":       "详见日志",
		"level":      "critical",
		"url":        "https://ci.example.com/1",
		"group":      "unipush",
	}
	for key, expected := range want {
		if got, _ := gotBody[key].(string); got != expected {
			t.Errorf("请求体 %s = %q, 期望 %q", key, got, expected)
		}
	}
}

func TestSendMapsLevels(t *testing.T) {
	cases := map[string]string{
		model.LevelInfo:  "active",
		model.LevelWarn:  "timeSensitive",
		model.LevelError: "critical",
	}
	for level, want := range cases {
		t.Run(level, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&gotBody)
				io.WriteString(w, `{"code":200,"message":"success"}`)
			}))
			defer srv.Close()

			if err := newTestSender(t, srv.URL).Send(context.Background(), &model.Message{Body: "x", Level: level}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got := gotBody["level"]; got != want {
				t.Errorf("level = %v, 期望 %s", got, want)
			}
		})
	}
}

func TestSendHTTPClientErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := newTestSender(t, srv.URL).Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if channel.IsPermanent(err) {
		t.Errorf("5xx 应可重试，实际被标记为不可重试: %v", err)
	}
}

func TestSendBadDeviceKeyIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":400,"message":"failed to get device token"}`)
	}))
	defer srv.Close()

	err := newTestSender(t, srv.URL).Send(context.Background(), &model.Message{Body: "x"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !channel.IsPermanent(err) {
		t.Errorf("device key 错误应不可重试: %v", err)
	}
}

func TestSendHTTP4xxIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	err := newTestSender(t, srv.URL).Send(context.Background(), &model.Message{Body: "x"})
	if err == nil || !channel.IsPermanent(err) {
		t.Fatalf("4xx 应为不可重试错误，实际: %v", err)
	}
}

func TestNewRequiresDeviceKey(t *testing.T) {
	if _, err := bark.New(bark.Config{DeviceKey: "  "}); err == nil {
		t.Fatal("device key 为空时应返回错误")
	}
}
