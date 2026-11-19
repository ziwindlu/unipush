package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"

	_ "modernc.org/sqlite"
)

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testMessage() *model.Message {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &model.Message{
		ID:        model.NewID("msg_"),
		Title:     "测试标题",
		Body:      "测试正文",
		Level:     model.LevelWarn,
		Source:    "unit-test",
		Targets:   []string{"bark", "dingtalk"},
		DedupeKey: "dedupe-1",
		Link:      "https://example.com/1",
		Extra:     map[string]any{"host": "mac-mini"},
		CreatedAt: now,
	}
}

func testDelivery(messageID, channel string, next time.Time) *model.Delivery {
	return &model.Delivery{
		ID:            model.NewID("dlv_"),
		MessageID:     messageID,
		Channel:       channel,
		Status:        model.DeliveryPending,
		MaxAttempts:   5,
		NextAttemptAt: &next,
		CreatedAt:     next,
		UpdatedAt:     next,
	}
}

func TestEnqueueAndRead(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	now := time.Now().UTC()
	deliveries := []*model.Delivery{
		testDelivery(msg.ID, "bark", now),
		testDelivery(msg.ID, "dingtalk", now),
	}
	if err := st.Enqueue(ctx, msg, deliveries); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := st.GetMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Title != msg.Title || got.Body != msg.Body || got.Level != msg.Level || got.Source != msg.Source {
		t.Errorf("消息字段不一致: %+v", got)
	}
	if len(got.Targets) != 2 || got.Targets[0] != "bark" {
		t.Errorf("targets 不一致: %#v", got.Targets)
	}
	if got.Extra["host"] != "mac-mini" {
		t.Errorf("extra 不一致: %#v", got.Extra)
	}
	if !got.CreatedAt.Equal(msg.CreatedAt) {
		t.Errorf("created_at 不一致: %s vs %s", got.CreatedAt, msg.CreatedAt)
	}

	list, err := st.ListDeliveries(ctx, msg.ID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("投递记录数 = %d, 期望 2", len(list))
	}
	if list[0].Channel != "bark" || list[1].Channel != "dingtalk" {
		t.Errorf("投递记录顺序不符合预期: %s, %s", list[0].Channel, list[1].Channel)
	}
	if list[0].Status != model.DeliveryPending || list[0].Attempts != 0 {
		t.Errorf("新投递记录状态错误: %+v", list[0])
	}
}

func TestGetMessageNotFound(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))
	if _, err := st.GetMessage(context.Background(), "msg_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, 实际 %v", err)
	}
}

func TestClaimDueLeasesAndIncrementsAttempts(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	now := time.Now().UTC()
	d := testDelivery(msg.ID, "bark", now)
	if err := st.Enqueue(ctx, msg, []*model.Delivery{d}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claimed, err := st.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("认领数量 = %d, 期望 1", len(claimed))
	}
	if claimed[0].Attempts != 1 || claimed[0].Status != model.DeliverySending {
		t.Errorf("认领后状态错误: %+v", claimed[0])
	}

	// 租约未到期，不应被重复认领。
	again, err := st.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("租约期内重复认领了 %d 条", len(again))
	}

	// 模拟进程重启：在途投递回到队列。
	n, err := st.RequeueInFlight(ctx)
	if err != nil {
		t.Fatalf("RequeueInFlight: %v", err)
	}
	if n != 1 {
		t.Fatalf("恢复数量 = %d, 期望 1", n)
	}
	reclaimed, err := st.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("恢复后认领错误: %+v", reclaimed)
	}
}

func TestClaimDueOnlyReturnsDueDeliveries(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	deliveries := []*model.Delivery{
		testDelivery(msg.ID, "bark", now.Add(-time.Second)),
		testDelivery(msg.ID, "dingtalk", future),
	}
	if err := st.Enqueue(ctx, msg, deliveries); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claimed, err := st.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Channel != "bark" {
		t.Fatalf("只应认领到期的 bark，实际: %+v", claimed)
	}
}

func TestMarkSucceededAndFailed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	now := time.Now().UTC()
	d1 := testDelivery(msg.ID, "bark", now)
	d2 := testDelivery(msg.ID, "dingtalk", now)
	if err := st.Enqueue(ctx, msg, []*model.Delivery{d1, d2}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := st.ClaimDue(ctx, 10, time.Minute); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}

	if err := st.MarkSucceeded(ctx, d1.ID); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	if err := st.MarkFailed(ctx, d2.ID, "鉴权失败"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	list, err := st.ListDeliveries(ctx, msg.ID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if list[0].Status != model.DeliverySucceeded || list[0].SucceededAt == nil {
		t.Errorf("bark 投递未正确标记成功: %+v", list[0])
	}
	if list[1].Status != model.DeliveryFailed || list[1].LastError != "鉴权失败" {
		t.Errorf("dingtalk 投递未正确标记失败: %+v", list[1])
	}
}

func TestReschedule(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	now := time.Now().UTC()
	d := testDelivery(msg.ID, "bark", now)
	if err := st.Enqueue(ctx, msg, []*model.Delivery{d}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := st.ClaimDue(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: n=%d err=%v", len(claimed), err)
	}

	next := time.Now().UTC().Add(-time.Millisecond) // 立刻再次到期
	if err := st.Reschedule(ctx, d.ID, next, "网络不可达"); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	again, err := st.ClaimDue(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(again) != 1 || again[0].Attempts != 2 {
		t.Fatalf("重新排队后认领错误: %+v", again)
	}
	if again[0].LastError != "网络不可达" {
		t.Errorf("last_error 未保留: %q", again[0].LastError)
	}
}

func TestQueueSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unipush.db")

	st1, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msg := testMessage()
	now := time.Now().UTC()
	d := testDelivery(msg.ID, "bark", now)
	if err := st1.Enqueue(ctx, msg, []*model.Delivery{d}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// 模拟进程在发送途中被 kill：认领后不写回就关闭。
	if _, err := st1.ClaimDue(ctx, 1, time.Minute); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer st2.Close()

	if n, err := st2.RequeueInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("重启恢复在途投递: n=%d err=%v", n, err)
	}
	claimed, err := st2.ClaimDue(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("重启后投递记录错误: %+v", claimed)
	}
	msg2, err := st2.GetMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("重启后读取消息: %v", err)
	}
	if msg2.Body != msg.Body {
		t.Errorf("重启后消息内容不一致: %q", msg2.Body)
	}
}

func TestRouteDecisionRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	msg.Route = &model.Route{
		Mode:     "rule",
		Rule:     "nas-alerts",
		Groups:   []string{"phone"},
		Channels: []string{"bark"},
	}
	now := time.Now().UTC()
	d := testDelivery(msg.ID, "bark", now)
	d.Rule = "nas-alerts"
	d.Groups = []string{"phone"}
	if err := st.Enqueue(ctx, msg, []*model.Delivery{d}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := st.GetMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Route == nil {
		t.Fatal("route 未落库")
	}
	if got.Route.Mode != "rule" || got.Route.Rule != "nas-alerts" {
		t.Errorf("route = %+v", got.Route)
	}
	if len(got.Route.Groups) != 1 || got.Route.Groups[0] != "phone" ||
		len(got.Route.Channels) != 1 || got.Route.Channels[0] != "bark" {
		t.Errorf("route 组/渠道不一致: %+v", got.Route)
	}

	list, err := st.ListDeliveries(ctx, msg.ID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("投递记录数 = %d", len(list))
	}
	if list[0].Rule != "nas-alerts" || len(list[0].Groups) != 1 || list[0].Groups[0] != "phone" {
		t.Errorf("投递记录的规则/组不一致: rule=%q groups=%v", list[0].Rule, list[0].Groups)
	}
}

func TestMessageWithoutRouteReadsBackNil(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))

	msg := testMessage()
	now := time.Now().UTC()
	if err := st.Enqueue(ctx, msg, []*model.Delivery{testDelivery(msg.ID, "bark", now)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := st.GetMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Route != nil {
		t.Errorf("没有路由信息的消息应读回 nil，实际 %+v", got.Route)
	}
}

// TestMigratesM1Database 验证 M1（schema v4）已有的库能平滑升级：
// 老消息读回时没有 route，新写入的投递记录带规则与组。
func TestMigratesM1Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unipush.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开旧库: %v", err)
	}
	m1Schema := []string{
		`CREATE TABLE messages (
			id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '',
			level TEXT NOT NULL, source TEXT NOT NULL DEFAULT '', targets TEXT NOT NULL DEFAULT '[]',
			dedupe_key TEXT NOT NULL DEFAULT '', link TEXT NOT NULL DEFAULT '',
			extra TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL)`,
		`CREATE TABLE deliveries (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			channel TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL, next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '', last_attempt_at INTEGER, succeeded_at INTEGER,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			UNIQUE (message_id, channel))`,
		`CREATE INDEX idx_deliveries_due ON deliveries(status, next_attempt_at)`,
		`CREATE INDEX idx_deliveries_message ON deliveries(message_id)`,
		`PRAGMA user_version = 4`,
	}
	for _, stmt := range m1Schema {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			raw.Close()
			t.Fatalf("初始化 M1 库 (%s): %v", stmt, err)
		}
	}
	now := time.Now().UTC()
	if _, err := raw.ExecContext(ctx, `INSERT INTO messages (id, title, body, level, source, created_at)
		VALUES ('msg_old', '旧消息', '正文', 'info', 'legacy', ?)`, now.UnixMilli()); err != nil {
		raw.Close()
		t.Fatalf("写入旧消息: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭旧库: %v", err)
	}

	st := openStore(t, path)

	old, err := st.GetMessage(ctx, "msg_old")
	if err != nil {
		t.Fatalf("升级后读取旧消息: %v", err)
	}
	if old.Route != nil {
		t.Errorf("旧消息不应有 route，实际 %+v", old.Route)
	}

	msg := testMessage()
	msg.Route = &model.Route{Mode: "rule", Rule: "nas-alerts", Groups: []string{"phone"}, Channels: []string{"bark"}}
	d := testDelivery(msg.ID, "bark", now)
	d.Rule = "nas-alerts"
	d.Groups = []string{"phone"}
	if err := st.Enqueue(ctx, msg, []*model.Delivery{d}); err != nil {
		t.Fatalf("升级后写入新消息: %v", err)
	}
	list, err := st.ListDeliveries(ctx, msg.ID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 1 || list[0].Rule != "nas-alerts" || len(list[0].Groups) != 1 {
		t.Fatalf("升级后投递记录不完整: %+v", list)
	}
}
