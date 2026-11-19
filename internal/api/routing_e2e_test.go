package api

import (
	"context"
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
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/routing"
	"github.com/ziwindlu/unipush/internal/store"
)

// countingSender 是记录调用次数的渠道 mock，用于验证「消息实际发到了哪里」。
type countingSender struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (s *countingSender) Name() string { return s.name }

func (s *countingSender) Send(ctx context.Context, msg *model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return nil
}

func (s *countingSender) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestEndToEndRoutingBySourceAndOverride 覆盖 M2 验收 1、2、4：
// 不同来源按规则投到不同渠道组合、显式 targets 覆盖规则、GET 能看到命中的规则与目标组。
func TestEndToEndRoutingBySourceAndOverride(t *testing.T) {
	barkSender := &countingSender{name: "bark"}
	dingtalkSender := &countingSender{name: "dingtalk"}

	router, err := routing.New(routing.Config{
		Groups: map[string][]string{
			"phone": {"bark"},
			"team":  {"dingtalk"},
			"all":   {"bark", "dingtalk"},
		},
		Default: []string{"all"},
		Rules: []routing.Rule{
			{Name: "nas-alerts", Sources: []string{"nas-*"}, Levels: []string{model.LevelError}, Groups: []string{"phone"}},
			{Name: "ci-build", Sources: []string{"ci"}, Levels: []string{model.LevelInfo}, Groups: []string{"team"}},
		},
	}, routing.Options{
		SupportedChannels: []string{"bark", "dingtalk"},
		EnabledChannels:   []string{"bark", "dingtalk"},
	})
	if err != nil {
		t.Fatalf("routing.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "unipush.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dsp := dispatch.New(st, channel.Set{"bark": barkSender, "dingtalk": dingtalkSender}, router, dispatch.Config{
		Workers:        2,
		PollInterval:   5 * time.Millisecond,
		Lease:          time.Minute,
		MaxAttempts:    5,
		BaseBackoff:    5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		RequestTimeout: time.Second,
	}, logger)

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

	ts := httptest.NewServer(New(Config{AuthTokens: []string{testToken}, MaxBodyBytes: 1 << 20}, st, dsp, logger).Handler())
	defer ts.Close()

	post := func(body string) createResponse {
		t.Helper()
		resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, body)
		if resp.StatusCode != http.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("创建消息状态码 = %d, body=%s", resp.StatusCode, raw)
		}
		var created createResponse
		decodeBody(t, resp, &created)
		return created
	}

	// 1. nas-monitor 的 error 只发手机（Bark）。
	nas := post(`{"title":"磁盘告警","body":"剩余空间 3%","level":"error","source":"nas-monitor"}`)
	if len(nas.Deliveries) != 1 || nas.Deliveries[0].Channel != "bark" {
		t.Fatalf("nas 告警应只排队 bark，实际 %+v", nas.Deliveries)
	}
	if nas.Route == nil || nas.Route.Rule != "nas-alerts" || strings.Join(nas.Route.Groups, ",") != "phone" {
		t.Fatalf("创建响应应带命中的规则与组: %+v", nas.Route)
	}

	nasFinal := waitForSucceeded(t, ts, nas.ID)
	if nasFinal.Message.Route == nil {
		t.Fatal("GET /v1/messages/{id} 应返回路由解析结果")
	}
	if nasFinal.Message.Route.Mode != routing.ModeRule || nasFinal.Message.Route.Rule != "nas-alerts" {
		t.Errorf("route = %+v, 期望命中规则 nas-alerts", nasFinal.Message.Route)
	}
	if strings.Join(nasFinal.Message.Route.Groups, ",") != "phone" {
		t.Errorf("route.groups = %v, 期望 [phone]", nasFinal.Message.Route.Groups)
	}
	if strings.Join(nasFinal.Message.Route.Channels, ",") != "bark" {
		t.Errorf("route.channels = %v, 期望 [bark]", nasFinal.Message.Route.Channels)
	}
	for _, d := range nasFinal.Deliveries {
		if d.Rule != "nas-alerts" || strings.Join(d.Groups, ",") != "phone" {
			t.Errorf("投递记录应带命中的规则与组: rule=%q groups=%v", d.Rule, d.Groups)
		}
	}

	// 2. ci 的 info 只发钉钉群，调用方不改代码。
	ci := post(`{"title":"构建完成","body":"v1.2.3 已上线","level":"info","source":"ci"}`)
	if len(ci.Deliveries) != 1 || ci.Deliveries[0].Channel != "dingtalk" {
		t.Fatalf("ci 信息应只排队 dingtalk，实际 %+v", ci.Deliveries)
	}
	if ci.Route == nil || ci.Route.Rule != "ci-build" || strings.Join(ci.Route.Groups, ",") != "team" {
		t.Fatalf("ci 应命中 ci-build → team: %+v", ci.Route)
	}
	waitForSucceeded(t, ts, ci.ID)

	// 3. 同一个 nas 来源显式传 targets=team，覆盖规则只发钉钉。
	override := post(`{"title":"磁盘告警","body":"剩余空间 3%","level":"error","source":"nas-monitor","targets":["team"]}`)
	if len(override.Deliveries) != 1 || override.Deliveries[0].Channel != "dingtalk" {
		t.Fatalf("显式 targets 应覆盖规则，实际 %+v", override.Deliveries)
	}
	if override.Route == nil || override.Route.Mode != routing.ModeExplicit || strings.Join(override.Route.Groups, ",") != "team" {
		t.Fatalf("route 应为 explicit → team: %+v", override.Route)
	}
	waitForSucceeded(t, ts, override.ID)

	// 显式覆盖没有额外打到 Bark：bark 只收到第 1 条。
	if got := barkSender.Calls(); got != 1 {
		t.Errorf("bark 调用次数 = %d, 期望 1", got)
	}
	if got := dingtalkSender.Calls(); got != 2 {
		t.Errorf("dingtalk 调用次数 = %d, 期望 2", got)
	}
}

func waitForSucceeded(t *testing.T, ts *httptest.Server, id string) getResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := requestJSON(t, ts, http.MethodGet, "/v1/messages/"+id, testToken, "")
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("查询消息状态码 = %d, body=%s", resp.StatusCode, raw)
		}
		var out getResponse
		decodeBody(t, resp, &out)
		if out.Status == "succeeded" {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待投递成功超时，当前: %+v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
