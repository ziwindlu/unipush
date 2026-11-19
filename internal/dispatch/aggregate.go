package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/model"
)

// aggregateBucket 决定一条 info 消息进哪个聚合桶。
//
// 桶键 = 来源 + targets：同来源且投递目标一致的消息才会合并成一条 digest，
// 避免把「只发手机」的消息和「发群」的消息拼在一起后重复投递。
func aggregateBucket(msg *model.Message) string {
	return strings.ToLower(strings.TrimSpace(msg.Source)) + "|" +
		strings.Join(model.NormalizeTargets(msg.Targets), ",")
}

// FlushDue 把到期的聚合桶汇总成 digest 并投递，返回本次生成的 digest 数量。
func (dsp *Dispatcher) FlushDue(ctx context.Context) (int, error) {
	if !dsp.agg.Enabled {
		return 0, nil
	}
	buckets, err := dsp.store.DueAggregateBuckets(ctx, dsp.agg.Window, dsp.agg.MaxItems, dsp.now())
	if err != nil {
		return 0, err
	}
	flushed := 0
	for _, bucket := range buckets {
		ok, err := dsp.flushBucket(ctx, bucket.Bucket)
		if err != nil {
			return flushed, err
		}
		if ok {
			flushed++
		}
	}
	return flushed, nil
}

// flushBucket 把一个桶里的消息合成一条 digest：先按 digest 路由生成投递记录，
// 再一次事务里写 digest、标记原始消息归属并清空桶。
func (dsp *Dispatcher) flushBucket(ctx context.Context, bucket string) (bool, error) {
	items, err := dsp.store.AggregateItems(ctx, bucket)
	if err != nil {
		return false, err
	}
	if len(items) == 0 {
		return false, nil
	}

	now := dsp.now()
	digest := buildDigest(items, now)
	channels, channelGroups, err := dsp.resolve(digest)
	if err != nil {
		return false, fmt.Errorf("digest 路由失败: %w", err)
	}
	deliveries := dsp.buildDeliveries(digest, channels, channelGroups, now)

	count, err := dsp.store.FlushAggregate(ctx, bucket, digest.ID, digest, deliveries)
	if err != nil {
		return false, err
	}
	if count == 0 {
		// 另一个 worker 已经把这个桶发走了。
		return false, nil
	}
	dsp.Wake()
	dsp.log.Info("info 消息已汇总成 digest",
		"digest_id", digest.ID,
		"source", digest.Source,
		"count", count,
		"channels", channels,
	)
	return true, nil
}

// aggregateLoop 周期性检查聚合桶：到窗口或攒够 max_items 就发 digest。
// 停机时把桶里剩下的消息也发掉，避免下次启动再等一个窗口。
func (dsp *Dispatcher) aggregateLoop(ctx context.Context) {
	ticker := time.NewTicker(dsp.flushEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if n, err := dsp.FlushDue(flushCtx); err != nil {
				dsp.log.Error("停机前汇总 digest 失败", "error", err)
			} else if n > 0 {
				dsp.log.Info("停机前已汇总剩余 info 消息", "digests", n)
			}
			cancel()
			return
		case <-ticker.C:
			n, err := dsp.FlushDue(ctx)
			if err != nil {
				if ctx.Err() == nil {
					dsp.log.Error("汇总 digest 失败", "error", err)
				}
				continue
			}
			if n > 0 {
				dsp.log.Info("已按窗口汇总 info 消息", "digests", n)
			}
		}
	}
}

// buildDigest 把同一个桶里的消息拼成一条 digest。
func buildDigest(items []*model.Message, now time.Time) *model.Message {
	source := items[0].Source
	label := source
	if label == "" {
		label = "info"
	}
	digest := &model.Message{
		ID:          model.NewID("msg_"),
		Title:       fmt.Sprintf("%d 条 %s 汇总", len(items), label),
		Level:       model.LevelInfo,
		Source:      source,
		Targets:     items[0].Targets,
		CreatedAt:   now,
		DigestCount: len(items),
	}

	lines := make([]string, 0, len(items))
	for _, item := range items {
		line := "• " + item.CreatedAt.Local().Format("15:04") + " "
		switch {
		case item.Title != "" && item.Body != "":
			line += item.Title + "：" + truncateRunes(item.Body, 120)
		case item.Title != "":
			line += item.Title
		default:
			line += truncateRunes(item.Body, 120)
		}
		lines = append(lines, line)
		if digest.Link == "" {
			digest.Link = item.Link
		}
	}
	digest.Body = strings.Join(lines, "\n")
	return digest
}

// truncateRunes 按字符数截断，避免切坏多字节字符。
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
