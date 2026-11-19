package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

func newMessage(id, source string, level string, at time.Time) *model.Message {
	return &model.Message{
		ID:        id,
		Title:     "标题 " + id,
		Body:      "正文",
		Level:     level,
		Source:    source,
		CreatedAt: at.UTC().Truncate(time.Millisecond),
	}
}

func enqueueWithStatus(t *testing.T, st *store.Store, msg *model.Message, channels ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var deliveries []*model.Delivery
	for _, ch := range channels {
		deliveries = append(deliveries, testDelivery(msg.ID, ch, now))
	}
	if err := st.Enqueue(ctx, msg, deliveries); err != nil {
		t.Fatalf("Enqueue %s: %v", msg.ID, err)
	}
}

func TestListMessagesFiltersAndStatus(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	queued := newMessage("msg_queued", "ci", model.LevelInfo, base)
	enqueueWithStatus(t, st, queued)

	aggregated := newMessage("msg_aggregated", "ci", model.LevelInfo, base.Add(time.Second))
	aggregated.AggregatedInto = "msg_digest"
	enqueueWithStatus(t, st, aggregated)

	pending := newMessage("msg_pending", "nas-monitor", model.LevelWarn, base.Add(2*time.Second))
	enqueueWithStatus(t, st, pending, "bark")

	succeeded := newMessage("msg_succeeded", "nas-monitor", model.LevelError, base.Add(3*time.Second))
	enqueueWithStatus(t, st, succeeded, "bark")
	if err := st.MarkSucceeded(ctx, deliveryID(t, st, succeeded.ID)); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	partial := newMessage("msg_partial", "ci", model.LevelError, base.Add(4*time.Second))
	enqueueWithStatus(t, st, partial, "bark", "dingtalk")
	ids := deliveryIDs(t, st, partial.ID)
	if err := st.MarkSucceeded(ctx, ids["bark"]); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	if err := st.MarkFailed(ctx, ids["dingtalk"], "rejected"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	failed := newMessage("msg_failed", "ci", model.LevelError, base.Add(5*time.Second))
	enqueueWithStatus(t, st, failed, "bark")
	if err := st.MarkFailed(ctx, deliveryID(t, st, failed.ID), "device key 无效"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	// 倒序：failed, partial, succeeded, pending, aggregated, queued。
	items, hasMore, err := st.ListMessages(ctx, store.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if hasMore {
		t.Error("6 条消息、limit 10 不应还有下一页")
	}
	var gotIDs []string
	for _, item := range items {
		gotIDs = append(gotIDs, item.Message.ID)
	}
	want := []string{"msg_failed", "msg_partial", "msg_succeeded", "msg_pending", "msg_aggregated", "msg_queued"}
	if len(gotIDs) != len(want) {
		t.Fatalf("列表条数 = %d, 期望 %d: %v", len(gotIDs), len(want), gotIDs)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("第 %d 条 = %s, 期望 %s", i, gotIDs[i], want[i])
		}
	}
	// 列表要带上投递摘要，方便直接看到哪个渠道失败。
	if items[0].Status != "failed" || len(items[0].Deliveries) != 1 || items[0].Deliveries[0].LastError == "" {
		t.Errorf("失败消息的列表项不完整: %+v", items[0])
	}

	statusCases := []struct{ status, want string }{
		{"queued", "msg_queued"},
		{"aggregated", "msg_aggregated"},
		{"in_progress", "msg_pending"},
		{"succeeded", "msg_succeeded"},
		{"partial", "msg_partial"},
		{"failed", "msg_failed"},
	}
	for _, tc := range statusCases {
		t.Run("status="+tc.status, func(t *testing.T) {
			items, _, err := st.ListMessages(ctx, store.MessageFilter{Status: tc.status, Limit: 10})
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			if len(items) != 1 || items[0].Message.ID != tc.want {
				t.Fatalf("status=%s 命中 %v, 期望只有 %s", tc.status, idsOf(items), tc.want)
			}
			if items[0].Status != tc.status {
				t.Errorf("列表项状态 = %s, 期望 %s", items[0].Status, tc.status)
			}
		})
	}

	bySource, _, err := st.ListMessages(ctx, store.MessageFilter{Source: "nas-monitor", Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(bySource) != 2 {
		t.Fatalf("source=nas-monitor 命中 %v", idsOf(bySource))
	}

	since := base.Add(4 * time.Second)
	recent, _, err := st.ListMessages(ctx, store.MessageFilter{Since: &since, Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(recent) != 2 || recent[0].Message.ID != "msg_failed" || recent[1].Message.ID != "msg_partial" {
		t.Fatalf("since 过滤结果不对: %v", idsOf(recent))
	}

	if _, _, err := st.ListMessages(ctx, store.MessageFilter{Status: "unknown"}); err == nil {
		t.Error("未知状态应返回错误")
	}
}

func TestListMessagesPagination(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	for i := 0; i < 3; i++ {
		msg := newMessage("msg_"+string(rune('a'+i)), "ci", model.LevelInfo, base.Add(time.Duration(i)*time.Second))
		enqueueWithStatus(t, st, msg)
	}

	first, hasMore, err := st.ListMessages(ctx, store.MessageFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if !hasMore || len(first) != 2 {
		t.Fatalf("第一页 hasMore=%v 条数=%d", hasMore, len(first))
	}
	if first[0].Message.ID != "msg_c" || first[1].Message.ID != "msg_b" {
		t.Fatalf("第一页顺序不对: %v", idsOf(first))
	}

	last := first[len(first)-1].Message
	second, hasMore, err := st.ListMessages(ctx, store.MessageFilter{
		Limit:  2,
		Before: &store.MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID},
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if hasMore || len(second) != 1 || second[0].Message.ID != "msg_a" {
		t.Fatalf("第二页不对: hasMore=%v %v", hasMore, idsOf(second))
	}
}

func TestDeliveryStats(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "unipush.db"))
	now := time.Now().UTC()

	msg := newMessage("msg_stats", "ci", model.LevelWarn, now)
	enqueueWithStatus(t, st, msg, "bark", "dingtalk", "feishu")
	ids := deliveryIDs(t, st, msg.ID)
	if err := st.MarkSucceeded(ctx, ids["bark"]); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	if err := st.MarkFailed(ctx, ids["dingtalk"], "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	stats, err := st.DeliveryStats(ctx)
	if err != nil {
		t.Fatalf("DeliveryStats: %v", err)
	}
	if stats.Pending != 1 || stats.Succeeded != 1 || stats.Failed != 1 || stats.Sending != 0 {
		t.Errorf("投递统计不对: %+v", stats)
	}
}

func idsOf(items []*store.MessageListItem) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Message.ID
	}
	return out
}

func deliveryIDs(t *testing.T, st *store.Store, messageID string) map[string]string {
	t.Helper()
	list, err := st.ListDeliveries(context.Background(), messageID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	out := make(map[string]string, len(list))
	for _, d := range list {
		out[d.Channel] = d.ID
	}
	return out
}

func deliveryID(t *testing.T, st *store.Store, messageID string) string {
	t.Helper()
	list, err := st.ListDeliveries(context.Background(), messageID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("消息 %s 有 %d 条投递，期望 1 条", messageID, len(list))
	}
	return list[0].ID
}
