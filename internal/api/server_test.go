package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

const testToken = "good-token"

type fakeStore struct {
	msg           *model.Message
	deliveries    []*model.Delivery
	aggregatedIDs []string
	getErr        error
	pingErr       error
	listItems     []*store.MessageListItem
	listHasMore   bool
	listErr       error
	queueStats    store.DeliveryQueueStats
	statsErr      error
	lastFilter    store.MessageFilter
}

func (f *fakeStore) GetMessage(ctx context.Context, id string) (*model.Message, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.msg == nil || f.msg.ID != id {
		return nil, store.ErrNotFound
	}
	return f.msg, nil
}

func (f *fakeStore) ListDeliveries(ctx context.Context, messageID string) ([]*model.Delivery, error) {
	return f.deliveries, nil
}

func (f *fakeStore) ListAggregatedMessages(ctx context.Context, digestID string) ([]string, error) {
	return f.aggregatedIDs, nil
}

func (f *fakeStore) ListMessages(ctx context.Context, filter store.MessageFilter) ([]*store.MessageListItem, bool, error) {
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	return f.listItems, f.listHasMore, nil
}

func (f *fakeStore) DeliveryStats(ctx context.Context) (store.DeliveryQueueStats, error) {
	return f.queueStats, f.statsErr
}

func (f *fakeStore) Ping(ctx context.Context) error { return f.pingErr }

type fakeQueue struct {
	mu       sync.Mutex
	channels []string
	groups   []string
	enqueued []*model.Message
	err      error
}

func (f *fakeQueue) Channels() []string { return f.channels }

func (f *fakeQueue) Groups() []string { return f.groups }

func (f *fakeQueue) Enqueue(ctx context.Context, msg *model.Message) (*model.EnqueueResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, msg)

	targets := msg.Targets
	if len(targets) == 0 {
		targets = f.channels
	}
	out := make([]*model.Delivery, 0, len(targets))
	for _, name := range targets {
		out = append(out, &model.Delivery{
			ID:      model.NewID("dlv_"),
			Channel: name,
			Status:  model.DeliveryPending,
		})
	}
	return &model.EnqueueResult{
		MessageID:  msg.ID,
		Status:     model.EnqueueQueued,
		Route:      msg.Route,
		Deliveries: out,
	}, nil
}

func (f *fakeQueue) lastEnqueued() *model.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.enqueued) == 0 {
		return nil
	}
	return f.enqueued[len(f.enqueued)-1]
}

func newTestAPI(t *testing.T, st Store, q Queue, mutate ...func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Config{AuthTokens: []string{testToken}, MaxBodyBytes: 1 << 20}
	for _, fn := range mutate {
		fn(&cfg)
	}
	server := New(cfg, st, q, discardLogger())
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func requestJSON(t *testing.T, ts *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("解析响应 %q: %v", raw, err)
	}
}

func TestEndpointsRequireValidToken(t *testing.T) {
	st := &fakeStore{msg: &model.Message{ID: "msg_1"}}
	q := &fakeQueue{channels: []string{"bark"}}
	ts := newTestAPI(t, st, q)

	cases := []struct {
		name  string
		token string
	}{
		{"无 token", ""},
		{"错误 token", "wrong-token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", c.token, `{"body":"x"}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("缺少 WWW-Authenticate: %q", got)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			if body.Error.Code != "unauthorized" {
				t.Errorf("error.code = %q", body.Error.Code)
			}
		})
	}

	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages/msg_1", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET 无 token 状态码 = %d, 期望 401", resp.StatusCode)
	}

	// 带上正确的 token 应放行。
	resp2 := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, `{"body":"x"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("有效 token 状态码 = %d, 期望 202", resp2.StatusCode)
	}
}

func TestCreateValidationErrors(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		field  string
	}{
		{"空请求体", "", http.StatusBadRequest, ""},
		{"非法 JSON", "{", http.StatusBadRequest, ""},
		{"title 与 body 都为空", `{"title":"  ","body":" "}`, http.StatusBadRequest, "title"},
		{"level 非法", `{"body":"x","level":"fatal"}`, http.StatusBadRequest, "level"},
		{"未知 targets", `{"body":"x","targets":["telegram"]}`, http.StatusBadRequest, "targets"},
		{"link 非 http", `{"body":"x","link":"ftp://example.com"}`, http.StatusBadRequest, "link"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQueue{channels: []string{"bark", "dingtalk"}}
			ts := newTestAPI(t, &fakeStore{}, q)
			resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, c.body)
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("状态码 = %d, 期望 %d, body=%s", resp.StatusCode, c.status, raw)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Field   string `json:"field"`
				} `json:"error"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			if body.Error.Message == "" {
				t.Error("错误响应必须说明原因")
			}
			if c.field != "" && body.Error.Field != c.field {
				t.Errorf("field = %q, 期望 %q (message=%q)", body.Error.Field, c.field, body.Error.Message)
			}
			if len(q.enqueued) != 0 {
				t.Errorf("校验失败不应入队，实际入队 %d 条", len(q.enqueued))
			}
		})
	}
}

func TestCreateRejectsOversizedBody(t *testing.T) {
	q := &fakeQueue{channels: []string{"bark"}}
	ts := newTestAPI(t, &fakeStore{}, q, func(c *Config) { c.MaxBodyBytes = 32 })
	resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken,
		`{"body":"`+strings.Repeat("x", 100)+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413", resp.StatusCode)
	}
}

func TestCreateWithoutChannelsReturns503(t *testing.T) {
	q := &fakeQueue{}
	ts := newTestAPI(t, &fakeStore{}, q)
	resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, `{"body":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", resp.StatusCode)
	}
}

func TestCreateNormalizesAndQueues(t *testing.T) {
	q := &fakeQueue{channels: []string{"bark", "dingtalk"}}
	ts := newTestAPI(t, &fakeStore{}, q)

	resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, `{
		"title": "  部署完成  ",
		"body": "  v1.2.3 已上线  ",
		"level": "ERROR",
		"source": "  ci  ",
		"targets": ["BARK", "bark"],
		"dedupe_key": "deploy-1",
		"link": "https://example.com/run/1",
		"extra": {"env": "prod"}
	}`)
	var respBody struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		CreatedAt  string `json:"created_at"`
		Deliveries []struct {
			ID      string `json:"id"`
			Channel string `json:"channel"`
			Status  string `json:"status"`
		} `json:"deliveries"`
	}
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d, 期望 202, body=%s", resp.StatusCode, raw)
	}
	decodeBody(t, resp, &respBody)
	if !strings.HasPrefix(respBody.ID, "msg_") {
		t.Errorf("id = %q, 期望 msg_ 前缀", respBody.ID)
	}
	if respBody.Status != "queued" {
		t.Errorf("status = %q, 期望 queued", respBody.Status)
	}
	if len(respBody.Deliveries) != 1 || respBody.Deliveries[0].Channel != "bark" {
		t.Fatalf("deliveries 不符合预期: %+v", respBody.Deliveries)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/messages/"+respBody.ID {
		t.Errorf("Location = %q", loc)
	}

	msg := q.lastEnqueued()
	if msg == nil {
		t.Fatal("消息未入队")
	}
	if msg.Title != "部署完成" || msg.Body != "v1.2.3 已上线" || msg.Source != "ci" {
		t.Errorf("字段未归一化: %+v", msg)
	}
	if msg.Level != model.LevelError {
		t.Errorf("level = %q, 期望 error", msg.Level)
	}
	if len(msg.Targets) != 1 || msg.Targets[0] != "bark" {
		t.Errorf("targets 未去重归一化: %#v", msg.Targets)
	}
	if msg.Extra["env"] != "prod" {
		t.Errorf("extra 未透传: %#v", msg.Extra)
	}
	if msg.CreatedAt.IsZero() {
		t.Error("created_at 未设置")
	}
}

func TestCreateDefaultsLevelToInfo(t *testing.T) {
	q := &fakeQueue{channels: []string{"bark"}}
	ts := newTestAPI(t, &fakeStore{}, q)
	resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, `{"body":"x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if msg := q.lastEnqueued(); msg == nil || msg.Level != model.LevelInfo {
		t.Fatalf("默认 level 应为 info: %+v", msg)
	}
}

func TestGetMessage(t *testing.T) {
	now := time.Now().UTC()
	msg := &model.Message{ID: "msg_1", Title: "t", Body: "b", Level: model.LevelInfo, CreatedAt: now}
	deliveries := []*model.Delivery{
		{ID: "dlv_1", MessageID: "msg_1", Channel: "bark", Status: model.DeliveryPending, NextAttemptAt: &now},
		{ID: "dlv_2", MessageID: "msg_1", Channel: "dingtalk", Status: model.DeliverySucceeded, SucceededAt: &now},
	}
	ts := newTestAPI(t, &fakeStore{msg: msg, deliveries: deliveries}, &fakeQueue{})

	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages/msg_1", testToken, "")
	var body getResponse
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, raw)
	}
	decodeBody(t, resp, &body)
	if body.Message.ID != "msg_1" || len(body.Deliveries) != 2 {
		t.Errorf("响应不符合预期: %+v", body)
	}
	if body.Status != "in_progress" {
		t.Errorf("聚合状态 = %q, 期望 in_progress", body.Status)
	}
}

func TestGetMessageNotFound(t *testing.T) {
	ts := newTestAPI(t, &fakeStore{}, &fakeQueue{})
	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages/msg_missing", testToken, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", resp.StatusCode)
	}
}

func TestAggregateStatus(t *testing.T) {
	cases := []struct {
		name       string
		msg        *model.Message
		deliveries []*model.Delivery
		want       string
	}{
		{"已汇入 digest", &model.Message{AggregatedInto: "msg_digest"}, nil, "aggregated"},
		{"空", nil, nil, "queued"},
		{"排队中", nil, []*model.Delivery{{Status: model.DeliveryPending}}, "in_progress"},
		{"发送中", nil, []*model.Delivery{{Status: model.DeliverySending}}, "in_progress"},
		{"部分成功", nil, []*model.Delivery{{Status: model.DeliverySucceeded}, {Status: model.DeliveryFailed}}, "partial"},
		{"全部失败", nil, []*model.Delivery{{Status: model.DeliveryFailed}, {Status: model.DeliveryFailed}}, "failed"},
		{"全部成功", nil, []*model.Delivery{{Status: model.DeliverySucceeded}, {Status: model.DeliverySucceeded}}, "succeeded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := aggregateStatus(c.msg, c.deliveries); got != c.want {
				t.Errorf("aggregateStatus = %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	ok := newTestAPI(t, &fakeStore{}, &fakeQueue{})
	resp := requestJSON(t, ok, http.MethodGet, "/healthz", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查状态码 = %d", resp.StatusCode)
	}

	bad := newTestAPI(t, &fakeStore{pingErr: fmt.Errorf("数据库不可用")}, &fakeQueue{})
	resp2 := requestJSON(t, bad, http.MethodGet, "/healthz", "", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("数据库异常时状态码 = %d, 期望 503", resp2.StatusCode)
	}
}

// TestHealthzReportsQueueAndVersion 覆盖 M4：健康检查除了数据库，还暴露版本与队列积压情况。
func TestHealthzReportsQueueAndVersion(t *testing.T) {
	st := &fakeStore{queueStats: store.DeliveryQueueStats{Pending: 2, Succeeded: 5}}
	ts := newTestAPI(t, st, &fakeQueue{channels: []string{"bark", "dingtalk"}}, func(c *Config) {
		c.Version = "v1.2.3"
	})
	resp := requestJSON(t, ts, http.MethodGet, "/healthz", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var body struct {
		Status  string            `json:"status"`
		Version string            `json:"version"`
		Checks  map[string]string `json:"checks"`
		Queue   struct {
			Pending   int `json:"pending"`
			Succeeded int `json:"succeeded"`
		} `json:"queue"`
		Channels []string `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body.Status != "ok" || body.Version != "v1.2.3" || body.Checks["database"] != "ok" {
		t.Errorf("健康检查字段不对: %+v", body)
	}
	if body.Queue.Pending != 2 || body.Queue.Succeeded != 5 {
		t.Errorf("队列统计不对: %+v", body.Queue)
	}
	if len(body.Channels) != 2 {
		t.Errorf("渠道列表不对: %+v", body.Channels)
	}
}
