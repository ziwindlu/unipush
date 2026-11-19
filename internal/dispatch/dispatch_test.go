package dispatch_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/routing"
	"github.com/ziwindlu/unipush/internal/store"
)

// fakeSender 是可编程的测试渠道：可指定前 N 次失败、永久失败或模拟断网。
type fakeSender struct {
	name      string
	mu        sync.Mutex
	calls     int
	failures  int
	permanent bool
	down      *atomic.Bool
}

func (f *fakeSender) Name() string { return f.name }

func (f *fakeSender) Send(ctx context.Context, msg *model.Message) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	if f.down != nil && f.down.Load() {
		return errors.New("网络不可达")
	}
	if call <= f.failures {
		if f.permanent {
			return channel.Permanent(errors.New("device key 无效"))
		}
		return errors.New("临时失败")
	}
	return nil
}

func (f *fakeSender) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fastConfig() dispatch.Config {
	return dispatch.Config{
		Workers:        2,
		PollInterval:   5 * time.Millisecond,
		Lease:          time.Second,
		MaxAttempts:    5,
		BaseBackoff:    5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		RequestTimeout: time.Second,
	}
}

type harness struct {
	st     *store.Store
	disp   *dispatch.Dispatcher
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func startHarness(t *testing.T, path string, senders channel.Set, cfg dispatch.Config) *harness {
	t.Helper()
	return startRoutedHarness(t, path, senders, nil, cfg)
}

func startRoutedHarness(t *testing.T, path string, senders channel.Set, router *routing.Router, cfg dispatch.Config) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	dsp := dispatch.New(st, senders, router, cfg, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{st: st, disp: dsp, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		dsp.Run(ctx)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.once.Do(func() {
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
		h.st.Close()
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待「%s」超时", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func deliveriesByChannel(t *testing.T, st *store.Store, messageID string) map[string]*model.Delivery {
	t.Helper()
	list, err := st.ListDeliveries(context.Background(), messageID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	out := make(map[string]*model.Delivery, len(list))
	for _, d := range list {
		out[d.Channel] = d
	}
	return out
}

func newMessage(targets ...string) *model.Message {
	return &model.Message{
		ID:      model.NewID("msg_"),
		Title:   "测试",
		Body:    "正文",
		Level:   model.LevelInfo,
		Source:  "dispatch-test",
		Targets: targets,
	}
}

func TestDeliversToAllChannels(t *testing.T) {
	bark := &fakeSender{name: "bark"}
	dingtalk := &fakeSender{name: "dingtalk"}
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"),
		channel.Set{"bark": bark, "dingtalk": dingtalk}, fastConfig())

	msg := newMessage()
	if _, err := h.disp.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 5*time.Second, "两个渠道投递成功", func() bool {
		ds := deliveriesByChannel(t, h.st, msg.ID)
		return ds["bark"] != nil && ds["bark"].Status == model.DeliverySucceeded &&
			ds["dingtalk"] != nil && ds["dingtalk"].Status == model.DeliverySucceeded
	})
	if bark.Calls() != 1 || dingtalk.Calls() != 1 {
		t.Errorf("每个渠道应各发送 1 次，实际 bark=%d dingtalk=%d", bark.Calls(), dingtalk.Calls())
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	bark := &fakeSender{name: "bark", failures: 2}
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"),
		channel.Set{"bark": bark}, fastConfig())

	msg := newMessage("bark")
	if _, err := h.disp.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 5*time.Second, "重试后成功", func() bool {
		d := deliveriesByChannel(t, h.st, msg.ID)["bark"]
		return d != nil && d.Status == model.DeliverySucceeded
	})
	ds := deliveriesByChannel(t, h.st, msg.ID)
	if ds["bark"].Attempts != 3 {
		t.Errorf("attempts = %d, 期望 3", ds["bark"].Attempts)
	}
	if bark.Calls() != 3 {
		t.Errorf("calls = %d, 期望 3", bark.Calls())
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	bark := &fakeSender{name: "bark", failures: 1 << 30}
	cfg := fastConfig()
	cfg.MaxAttempts = 3
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"), channel.Set{"bark": bark}, cfg)

	msg := newMessage("bark")
	if _, err := h.disp.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 5*time.Second, "重试耗尽后标记失败", func() bool {
		d := deliveriesByChannel(t, h.st, msg.ID)["bark"]
		return d != nil && d.Status == model.DeliveryFailed
	})
	ds := deliveriesByChannel(t, h.st, msg.ID)
	if ds["bark"].Attempts != 3 {
		t.Errorf("attempts = %d, 期望 3", ds["bark"].Attempts)
	}
	if ds["bark"].LastError == "" {
		t.Error("失败原因应写入 last_error")
	}
	if bark.Calls() != 3 {
		t.Errorf("calls = %d, 期望 3", bark.Calls())
	}
}

func TestPermanentErrorFailsImmediately(t *testing.T) {
	bark := &fakeSender{name: "bark", failures: 1 << 30, permanent: true}
	cfg := fastConfig()
	cfg.MaxAttempts = 5
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"), channel.Set{"bark": bark}, cfg)

	msg := newMessage("bark")
	if _, err := h.disp.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 5*time.Second, "永久失败立即标记", func() bool {
		d := deliveriesByChannel(t, h.st, msg.ID)["bark"]
		return d != nil && d.Status == model.DeliveryFailed
	})
	if bark.Calls() != 1 {
		t.Errorf("永久失败不应重试，实际调用 %d 次", bark.Calls())
	}
}

func TestOfflineKeepsQueuedThenRecovers(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	bark := &fakeSender{name: "bark", down: &down}
	cfg := fastConfig()
	cfg.MaxAttempts = 10
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"), channel.Set{"bark": bark}, cfg)

	msg := newMessage("bark")
	if _, err := h.disp.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// 断网期间：投递仍留在队列，而不是被丢弃或标记成功。
	waitFor(t, 5*time.Second, "断网期间至少重试两次", func() bool {
		d := deliveriesByChannel(t, h.st, msg.ID)["bark"]
		return d != nil && d.Attempts >= 2 && d.Status == model.DeliveryPending
	})
	if ds := deliveriesByChannel(t, h.st, msg.ID); ds["bark"].LastError == "" {
		t.Error("断网失败原因应写入 last_error")
	}

	down.Store(false)
	waitFor(t, 5*time.Second, "网络恢复后自动重投成功", func() bool {
		d := deliveriesByChannel(t, h.st, msg.ID)["bark"]
		return d != nil && d.Status == model.DeliverySucceeded
	})
	if ds := deliveriesByChannel(t, h.st, msg.ID); ds["bark"].LastError != "" {
		t.Errorf("成功后 last_error 应清空，实际 %q", ds["bark"].LastError)
	}
}

func TestRestartResumesPendingWithoutResendingSucceeded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unipush.db")

	bark1 := &fakeSender{name: "bark", failures: 1 << 30}
	dingtalk1 := &fakeSender{name: "dingtalk"}
	h1 := startHarness(t, path, channel.Set{"bark": bark1, "dingtalk": dingtalk1}, fastConfig())

	msg := newMessage()
	if _, err := h1.disp.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 5*time.Second, "dingtalk 成功且 bark 至少重试一次", func() bool {
		ds := deliveriesByChannel(t, h1.st, msg.ID)
		return ds["dingtalk"] != nil && ds["dingtalk"].Status == model.DeliverySucceeded &&
			ds["bark"] != nil && ds["bark"].Attempts >= 2 && ds["bark"].Status == model.DeliveryPending
	})
	h1.stop()

	bark2 := &fakeSender{name: "bark"}
	dingtalk2 := &fakeSender{name: "dingtalk"}
	h2 := startHarness(t, path, channel.Set{"bark": bark2, "dingtalk": dingtalk2}, fastConfig())

	waitFor(t, 5*time.Second, "重启后 bark 投递成功", func() bool {
		d := deliveriesByChannel(t, h2.st, msg.ID)["bark"]
		return d != nil && d.Status == model.DeliverySucceeded
	})
	waitFor(t, 5*time.Second, "所有投递完成", func() bool {
		ds := deliveriesByChannel(t, h2.st, msg.ID)
		return ds["bark"].Status == model.DeliverySucceeded && ds["dingtalk"].Status == model.DeliverySucceeded
	})
	if dingtalk2.Calls() != 0 {
		t.Errorf("已成功的 dingtalk 渠道在重启后被重复发送 %d 次", dingtalk2.Calls())
	}
	if bark2.Calls() != 1 {
		t.Errorf("重启后 bark 应只补发 1 次，实际 %d 次", bark2.Calls())
	}
	if ds := deliveriesByChannel(t, h2.st, msg.ID); ds["bark"].Attempts < 3 {
		t.Errorf("bark 的 attempts 应累计，实际 %d", ds["bark"].Attempts)
	}
}

func TestRestartRecoversInFlightDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unipush.db")

	st1, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	dsp1 := dispatch.New(st1, channel.Set{"bark": &fakeSender{name: "bark"}}, nil, fastConfig(), discardLogger())
	msg := newMessage("bark")
	if _, err := dsp1.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// 模拟「认领后进程被 kill」：投递停留在 sending。
	if claimed, err := st1.ClaimDue(context.Background(), 1, time.Minute); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: n=%d err=%v", len(claimed), err)
	}
	st1.Close()

	bark := &fakeSender{name: "bark"}
	h := startHarness(t, path, channel.Set{"bark": bark}, fastConfig())
	waitFor(t, 5*time.Second, "在途投递被恢复并成功", func() bool {
		d := deliveriesByChannel(t, h.st, msg.ID)["bark"]
		return d != nil && d.Status == model.DeliverySucceeded
	})
	if ds := deliveriesByChannel(t, h.st, msg.ID); ds["bark"].Attempts != 2 {
		t.Errorf("恢复后 attempts = %d, 期望 2", ds["bark"].Attempts)
	}
}

func TestEnqueueRejectsUnknownChannel(t *testing.T) {
	bark := &fakeSender{name: "bark"}
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"), channel.Set{"bark": bark}, fastConfig())
	if _, err := h.disp.Enqueue(context.Background(), newMessage("telegram")); err == nil {
		t.Fatal("未知渠道应返回错误")
	}
}

func TestEnqueueWithoutChannels(t *testing.T) {
	h := startHarness(t, filepath.Join(t.TempDir(), "unipush.db"), channel.Set{}, fastConfig())
	if _, err := h.disp.Enqueue(context.Background(), newMessage()); !errors.Is(err, dispatch.ErrNoChannels) {
		t.Fatalf("期望 ErrNoChannels，实际 %v", err)
	}
}

// testRouter 构造 M2 的路由表：nas 告警只发手机，ci 构建信息只发钉钉，其余全渠道。
func testRouter(t *testing.T, enabled ...string) *routing.Router {
	t.Helper()
	if enabled == nil {
		enabled = []string{"bark", "dingtalk"}
	}
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
		EnabledChannels:   enabled,
	})
	if err != nil {
		t.Fatalf("routing.New: %v", err)
	}
	return router
}

func routedMessage(source, level string, targets ...string) *model.Message {
	return &model.Message{
		ID:      model.NewID("msg_"),
		Title:   "测试",
		Body:    "正文",
		Level:   level,
		Source:  source,
		Targets: targets,
	}
}

func TestRoutingSelectsChannelsBySource(t *testing.T) {
	bark := &fakeSender{name: "bark"}
	dingtalk := &fakeSender{name: "dingtalk"}
	h := startRoutedHarness(t, filepath.Join(t.TempDir(), "unipush.db"),
		channel.Set{"bark": bark, "dingtalk": dingtalk}, testRouter(t), fastConfig())

	nas := routedMessage("nas-monitor", model.LevelError)
	deliveries, err := h.disp.Enqueue(context.Background(), nas)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Channel != "bark" {
		t.Fatalf("nas 告警应只投 bark，实际 %+v", deliveries)
	}
	if deliveries[0].Rule != "nas-alerts" || strings.Join(deliveries[0].Groups, ",") != "phone" {
		t.Errorf("投递记录应带命中规则与组: rule=%q groups=%v", deliveries[0].Rule, deliveries[0].Groups)
	}
	waitFor(t, 5*time.Second, "bark 投递成功", func() bool {
		d := deliveriesByChannel(t, h.st, nas.ID)["bark"]
		return d != nil && d.Status == model.DeliverySucceeded
	})

	stored, err := h.st.GetMessage(context.Background(), nas.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if stored.Route == nil {
		t.Fatal("消息没有保存路由解析结果")
	}
	if stored.Route.Mode != routing.ModeRule || stored.Route.Rule != "nas-alerts" {
		t.Errorf("route = %+v, 期望命中 nas-alerts", stored.Route)
	}
	if strings.Join(stored.Route.Groups, ",") != "phone" || strings.Join(stored.Route.Channels, ",") != "bark" {
		t.Errorf("route 目标组/渠道错误: %+v", stored.Route)
	}
	if stored.Route.Rule != deliveries[0].Rule {
		t.Errorf("消息与投递记录的规则不一致: %q vs %q", stored.Route.Rule, deliveries[0].Rule)
	}

	ci := routedMessage("ci", model.LevelInfo)
	deliveries, err = h.disp.Enqueue(context.Background(), ci)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Channel != "dingtalk" {
		t.Fatalf("ci 信息应只投 dingtalk，实际 %+v", deliveries)
	}
	if deliveries[0].Rule != "ci-build" || strings.Join(deliveries[0].Groups, ",") != "team" {
		t.Errorf("投递记录应带命中规则与组: rule=%q groups=%v", deliveries[0].Rule, deliveries[0].Groups)
	}
}

func TestRoutingExplicitTargetsOverrideRules(t *testing.T) {
	bark := &fakeSender{name: "bark"}
	dingtalk := &fakeSender{name: "dingtalk"}
	h := startRoutedHarness(t, filepath.Join(t.TempDir(), "unipush.db"),
		channel.Set{"bark": bark, "dingtalk": dingtalk}, testRouter(t), fastConfig())

	// 来源规则本该投 phone，显式 targets 指向 team 后覆盖规则。
	msg := routedMessage("nas-monitor", model.LevelError, "team")
	deliveries, err := h.disp.Enqueue(context.Background(), msg)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Channel != "dingtalk" {
		t.Fatalf("显式 targets 应覆盖规则，实际 %+v", deliveries)
	}
	if msg.Route == nil || msg.Route.Mode != routing.ModeExplicit {
		t.Fatalf("route.mode 应为 explicit: %+v", msg.Route)
	}
	if strings.Join(msg.Route.Groups, ",") != "team" {
		t.Errorf("route.groups = %v, 期望 [team]", msg.Route.Groups)
	}
}

func TestRoutingUnknownTarget(t *testing.T) {
	h := startRoutedHarness(t, filepath.Join(t.TempDir(), "unipush.db"),
		channel.Set{"bark": &fakeSender{name: "bark"}, "dingtalk": &fakeSender{name: "dingtalk"}},
		testRouter(t), fastConfig())

	_, err := h.disp.Enqueue(context.Background(), routedMessage("manual", model.LevelInfo, "telegram"))
	var unknown *routing.UnknownTargetError
	if !errors.As(err, &unknown) {
		t.Fatalf("期望 UnknownTargetError，实际 %v", err)
	}
}

func TestRoutingNoUsableChannel(t *testing.T) {
	// bark 不可用，只有 dingtalk；nas 告警规则指向 phone → 无路可走。
	h := startRoutedHarness(t, filepath.Join(t.TempDir(), "unipush.db"),
		channel.Set{"dingtalk": &fakeSender{name: "dingtalk"}},
		testRouter(t, "dingtalk"), fastConfig())

	_, err := h.disp.Enqueue(context.Background(), routedMessage("nas-monitor", model.LevelError))
	var noRoute *routing.NoRouteError
	if !errors.As(err, &noRoute) {
		t.Fatalf("期望 NoRouteError，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "phone") {
		t.Errorf("错误信息应指出目标组 phone: %s", err.Error())
	}
}
