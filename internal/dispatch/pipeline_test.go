package dispatch_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

// newPipeline 构造一个只做入队与聚合、不启动 worker 的 Dispatcher，
// 这样测试可以确定性地决定「什么时候 flush」。
func newPipeline(t *testing.T, cfg dispatch.Config) (*dispatch.Dispatcher, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "unipush.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	senders := channel.Set{"bark": &fakeSender{name: "bark"}, "dingtalk": &fakeSender{name: "dingtalk"}}
	dsp := dispatch.New(st, senders, testRouter(t), cfg, discardLogger())
	return dsp, st
}

func pipelineConfig() dispatch.Config {
	cfg := fastConfig()
	cfg.Dedupe = dispatch.DedupeConfig{Enabled: true, Window: 5 * time.Minute}
	cfg.Aggregation = dispatch.AggregationConfig{Enabled: false}
	return cfg
}

// TestDedupeKeyMergesWithinWindow 覆盖 M3 验收 2：
// 同一 dedupe_key 连发 10 次只产生 1 次投递，记录里显示合并了 9 次。
func TestDedupeKeyMergesWithinWindow(t *testing.T) {
	dsp, st := newPipeline(t, pipelineConfig())
	ctx := context.Background()

	var firstID string
	for i := 0; i < 10; i++ {
		msg := &model.Message{
			ID:        model.NewID("msg_"),
			Title:     "磁盘快满了",
			Level:     model.LevelWarn,
			Source:    "nas-monitor",
			Targets:   []string{"phone"},
			DedupeKey: "nas-disk-full",
		}
		res, err := dsp.Enqueue(ctx, msg)
		if err != nil {
			t.Fatalf("第 %d 次 Enqueue: %v", i+1, err)
		}
		if i == 0 {
			if res.Status != model.EnqueueQueued {
				t.Fatalf("首条消息状态 = %q，期望 queued", res.Status)
			}
			if len(res.Deliveries) != 1 {
				t.Fatalf("首条消息应产生 1 条投递，实际 %d", len(res.Deliveries))
			}
			firstID = res.MessageID
			continue
		}
		if res.Status != model.EnqueueDeduped {
			t.Fatalf("第 %d 次应命中去重，实际 %q", i+1, res.Status)
		}
		if res.MessageID != firstID {
			t.Errorf("去重后应返回原消息 ID，得到 %q，期望 %q", res.MessageID, firstID)
		}
		if res.Merged != i {
			t.Errorf("第 %d 次合并次数 = %d，期望 %d", i+1, res.Merged, i)
		}
		if len(res.Deliveries) != 0 {
			t.Errorf("命中去重不应产生新投递，实际 %d 条", len(res.Deliveries))
		}
	}

	stored, err := st.GetMessage(ctx, firstID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if stored.MergedCount != 9 {
		t.Errorf("消息记录里的合并次数 = %d，期望 9", stored.MergedCount)
	}
	deliveries, err := st.ListDeliveries(ctx, firstID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Errorf("总共应只有 1 条投递记录，实际 %d", len(deliveries))
	}
}

// TestDedupeWindowExpires 保证窗口外的同 key 消息会重新投递。
func TestDedupeWindowExpires(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Dedupe.Window = 40 * time.Millisecond
	dsp, _ := newPipeline(t, cfg)
	ctx := context.Background()

	newMsg := func() *model.Message {
		return &model.Message{
			ID:        model.NewID("msg_"),
			Title:     "构建失败",
			Level:     model.LevelWarn,
			Source:    "ci",
			DedupeKey: "ci-failed",
		}
	}
	first, err := dsp.Enqueue(ctx, newMsg())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	second, err := dsp.Enqueue(ctx, newMsg())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if second.Status != model.EnqueueQueued || second.MessageID == first.MessageID {
		t.Errorf("窗口过期后应作为新消息投递，实际 status=%q id=%q", second.Status, second.MessageID)
	}
}

// TestInfoMessagesAggregateIntoDigest 覆盖 M3 验收 3：
// info 级消息在窗口内合并成一条 digest，窗口到了才发送；原始消息记录 digest 归属。
func TestInfoMessagesAggregateIntoDigest(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Aggregation = dispatch.AggregationConfig{Enabled: true, Window: 60 * time.Millisecond, MaxItems: 10}
	dsp, st := newPipeline(t, cfg)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 3; i++ {
		msg := &model.Message{
			ID:     model.NewID("msg_"),
			Title:  fmt.Sprintf("备份任务 %d 完成", i),
			Body:   "耗时 32s",
			Level:  model.LevelInfo,
			Source: "backup",
		}
		res, err := dsp.Enqueue(ctx, msg)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if res.Status != model.EnqueueAggregated {
			t.Fatalf("info 消息应进入聚合窗口，实际 %q", res.Status)
		}
		if len(res.Deliveries) != 0 {
			t.Errorf("聚合中的消息不应立即产生投递，实际 %d 条", len(res.Deliveries))
		}
		ids = append(ids, msg.ID)
	}

	if n, err := dsp.FlushDue(ctx); err != nil || n != 0 {
		t.Fatalf("窗口未到时不应发 digest，n=%d err=%v", n, err)
	}

	time.Sleep(90 * time.Millisecond)
	n, err := dsp.FlushDue(ctx)
	if err != nil {
		t.Fatalf("FlushDue: %v", err)
	}
	if n != 1 {
		t.Fatalf("应生成 1 条 digest，实际 %d", n)
	}

	item, err := st.GetMessage(ctx, ids[0])
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if item.AggregatedInto == "" {
		t.Fatal("原始消息应记录 aggregated_into")
	}
	digest, err := st.GetMessage(ctx, item.AggregatedInto)
	if err != nil {
		t.Fatalf("读取 digest: %v", err)
	}
	if digest.DigestCount != 3 {
		t.Errorf("digest 汇总条数 = %d，期望 3", digest.DigestCount)
	}
	if digest.Level != model.LevelInfo || digest.Source != "backup" {
		t.Errorf("digest 级别/来源错误: %+v", digest)
	}
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf("备份任务 %d 完成", i)
		if !strings.Contains(digest.Body, want) {
			t.Errorf("digest 正文缺少 %q：%s", want, digest.Body)
		}
	}
	deliveries, err := st.ListDeliveries(ctx, digest.ID)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("digest 应按默认路由投 2 个渠道（bark + dingtalk），实际 %d", len(deliveries))
	}
}

// TestAggregationFlushesAtMaxItems 保证攒够条数立即发送，不必等窗口。
func TestAggregationFlushesAtMaxItems(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Aggregation = dispatch.AggregationConfig{Enabled: true, Window: 10 * time.Minute, MaxItems: 2}
	dsp, _ := newPipeline(t, cfg)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		msg := &model.Message{
			ID:     model.NewID("msg_"),
			Title:  "定时任务完成",
			Level:  model.LevelInfo,
			Source: "cron",
		}
		if _, err := dsp.Enqueue(ctx, msg); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	n, err := dsp.FlushDue(ctx)
	if err != nil {
		t.Fatalf("FlushDue: %v", err)
	}
	if n != 1 {
		t.Fatalf("攒够 max_items 应立即生成 digest，实际 %d", n)
	}
}

// TestErrorMessagesAreNotAggregated 保证 error 级消息不聚合、立即投递。
func TestErrorMessagesAreNotAggregated(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Aggregation = dispatch.AggregationConfig{Enabled: true, Window: time.Minute, MaxItems: 50}
	dsp, _ := newPipeline(t, cfg)

	msg := &model.Message{
		ID:     model.NewID("msg_"),
		Title:  "服务不可用",
		Level:  model.LevelError,
		Source: "api",
	}
	res, err := dsp.Enqueue(context.Background(), msg)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res.Status != model.EnqueueQueued {
		t.Fatalf("error 消息应直接投递，实际 %q", res.Status)
	}
	if len(res.Deliveries) == 0 {
		t.Fatal("error 消息应立即产生投递记录")
	}
}

// TestAggregationKeepsDifferentTargetsApart 保证投递目标不同的 info 消息不会被拼进同一条 digest。
func TestAggregationKeepsDifferentTargetsApart(t *testing.T) {
	cfg := pipelineConfig()
	cfg.Aggregation = dispatch.AggregationConfig{Enabled: true, Window: 20 * time.Millisecond, MaxItems: 10}
	dsp, _ := newPipeline(t, cfg)
	ctx := context.Background()

	phone := &model.Message{ID: model.NewID("msg_"), Title: "手机专用", Level: model.LevelInfo, Source: "nas", Targets: []string{"phone"}}
	team := &model.Message{ID: model.NewID("msg_"), Title: "群专用", Level: model.LevelInfo, Source: "nas", Targets: []string{"team"}}
	for _, msg := range []*model.Message{phone, team} {
		if _, err := dsp.Enqueue(ctx, msg); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	time.Sleep(40 * time.Millisecond)
	n, err := dsp.FlushDue(ctx)
	if err != nil {
		t.Fatalf("FlushDue: %v", err)
	}
	if n != 2 {
		t.Errorf("不同 targets 应分成 2 条 digest，实际 %d", n)
	}
}
