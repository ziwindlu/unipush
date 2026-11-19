package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/bark"
	"github.com/ziwindlu/unipush/internal/channel/dingtalk"
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/store"
)

type barkRecorder struct {
	mu      sync.Mutex
	calls   int
	payload map[string]any
}

func (r *barkRecorder) snapshot() (int, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.payload
}

type dingtalkRecorder struct {
	mu        sync.Mutex
	calls     int
	content   string
	timestamp string
	sign      string
	token     string
}

func (r *dingtalkRecorder) snapshot() (int, string, string, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.content, r.timestamp, r.sign, r.token
}

// TestEndToEndDeliversToBarkAndDingTalk 覆盖验收第 1 条：
// 一条 HTTP 消息同时触发 Bark 和钉钉投递，且两个 mock 收到的请求体、加签正确。
func TestEndToEndDeliversToBarkAndDingTalk(t *testing.T) {
	barkRec := &barkRecorder{}
	barkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/push" {
			t.Errorf("bark mock 收到非预期请求: %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		barkRec.mu.Lock()
		barkRec.calls++
		barkRec.payload = payload
		barkRec.mu.Unlock()
		io.WriteString(w, `{"code":200,"message":"success"}`)
	}))
	defer barkSrv.Close()

	const secret = "test-dingtalk-secret"
	dingRec := &dingtalkRecorder{}
	dingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		q := r.URL.Query()
		dingRec.mu.Lock()
		dingRec.calls++
		dingRec.content = payload.Text.Content
		dingRec.timestamp = q.Get("timestamp")
		dingRec.sign = q.Get("sign")
		dingRec.token = q.Get("access_token")
		dingRec.mu.Unlock()
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer dingSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "unipush.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	barkSender, err := bark.New(bark.Config{ServerURL: barkSrv.URL, DeviceKey: "key-1", Group: "unipush"})
	if err != nil {
		t.Fatalf("bark.New: %v", err)
	}
	dingSender, err := dingtalk.New(dingtalk.Config{
		Webhook: dingSrv.URL + "/robot/send?access_token=robot_token_123",
		Secret:  secret,
	})
	if err != nil {
		t.Fatalf("dingtalk.New: %v", err)
	}

	dsp := dispatch.New(st, channel.Set{"bark": barkSender, "dingtalk": dingSender}, nil, dispatch.Config{
		Workers:        2,
		PollInterval:   5 * time.Millisecond,
		Lease:          time.Minute,
		MaxAttempts:    5,
		BaseBackoff:    5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		RequestTimeout: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dsp.Run(dispatchCtx)
	}()
	defer func() {
		stopDispatch()
		<-dispatchDone
	}()

	server := New(Config{AuthTokens: []string{testToken}, MaxBodyBytes: 1 << 20}, st, dsp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	requestBody := `{
		"title": "部署完成",
		"body": "v1.2.3 已上线",
		"level": "warn",
		"source": "ci",
		"link": "https://example.com/run/1"
	}`
	resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, requestBody)
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("创建消息状态码 = %d, body=%s", resp.StatusCode, raw)
	}
	var created createResponse
	decodeBody(t, resp, &created)
	if len(created.Deliveries) != 2 {
		t.Fatalf("应同时排队到 2 个渠道，实际 %+v", created.Deliveries)
	}

	// 轮询投递状态，直到两个渠道都成功。
	var final getResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		getResp := requestJSON(t, ts, http.MethodGet, "/v1/messages/"+created.ID, testToken, "")
		if getResp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(getResp.Body)
			getResp.Body.Close()
			t.Fatalf("查询消息状态码 = %d, body=%s", getResp.StatusCode, raw)
		}
		decodeBody(t, getResp, &final)
		if final.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待投递成功超时，当前状态: %+v", final)
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, d := range final.Deliveries {
		if d.Status != "succeeded" {
			t.Errorf("渠道 %s 状态 = %s, 期望 succeeded", d.Channel, d.Status)
		}
		if d.Attempts != 1 {
			t.Errorf("渠道 %s attempts = %d, 期望 1", d.Channel, d.Attempts)
		}
	}

	calls, payload := barkRec.snapshot()
	if calls != 1 {
		t.Fatalf("bark 请求次数 = %d, 期望 1", calls)
	}
	wantBark := map[string]string{
		"title": "部署完成",
		"body":  "v1.2.3 已上线",
		"level": "timeSensitive",
		"url":   "https://example.com/run/1",
	}
	for key, want := range wantBark {
		if got, _ := payload[key].(string); got != want {
			t.Errorf("bark 请求体 %s = %q, 期望 %q", key, got, want)
		}
	}

	calls, content, timestamp, sign, token := dingRec.snapshot()
	if calls != 1 {
		t.Fatalf("dingtalk 请求次数 = %d, 期望 1", calls)
	}
	if want := dingtalk.Sign(secret, timestamp); sign != want {
		t.Errorf("钉钉签名 = %q, 期望 %q", sign, want)
	}
	if timestamp == "" {
		t.Error("钉钉请求缺少 timestamp")
	}
	if token != "robot_token_123" {
		t.Errorf("钉钉 access_token = %q", token)
	}
	for _, want := range []string{"部署完成", "v1.2.3 已上线", "https://example.com/run/1"} {
		if !strings.Contains(content, want) {
			t.Errorf("钉钉消息缺少 %q, 实际: %q", want, content)
		}
	}
}
