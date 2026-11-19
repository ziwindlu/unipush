// Package dispatch 负责把落库的消息投递到各渠道，并管理重试与失败。
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

// ErrNoChannels 表示服务没有启用任何可用渠道。
var ErrNoChannels = errors.New("没有可用渠道")

// Config 是投递 worker 配置。
type Config struct {
	Workers        int
	PollInterval   time.Duration
	Lease          time.Duration
	MaxAttempts    int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	RequestTimeout time.Duration
}

// DefaultConfig 返回可直接使用的默认配置。
func DefaultConfig() Config {
	return Config{
		Workers:        4,
		PollInterval:   time.Second,
		Lease:          60 * time.Second,
		MaxAttempts:    8,
		BaseBackoff:    2 * time.Second,
		MaxBackoff:     5 * time.Minute,
		RequestTimeout: 15 * time.Second,
	}
}

// Dispatcher 从 SQLite 队列认领投递并调用渠道适配器。
type Dispatcher struct {
	store   *store.Store
	senders channel.Set
	cfg     Config
	log     *slog.Logger
	wake    chan struct{}
	now     func() time.Time
}

// New 创建 Dispatcher。
func New(st *store.Store, senders channel.Set, cfg Config, logger *slog.Logger) *Dispatcher {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = time.Second
	}
	if cfg.MaxBackoff < cfg.BaseBackoff {
		cfg.MaxBackoff = cfg.BaseBackoff
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		store:   st,
		senders: senders,
		cfg:     cfg,
		log:     logger,
		wake:    make(chan struct{}, 1),
		now:     time.Now,
	}
}

// Channels 返回当前启用的渠道名（已排序）。
func (dsp *Dispatcher) Channels() []string { return dsp.senders.Names() }

// Wake 通知 worker 有新消息或可以继续认领，不阻塞调用方。
func (dsp *Dispatcher) Wake() {
	select {
	case dsp.wake <- struct{}{}:
	default:
	}
}

// Enqueue 为消息创建各渠道的投递记录并落库。targets 为空表示投递到全部启用渠道。
func (dsp *Dispatcher) Enqueue(ctx context.Context, msg *model.Message) ([]*model.Delivery, error) {
	available := dsp.senders.Names()
	targets := model.NormalizeTargets(msg.Targets)
	if len(targets) == 0 {
		targets = available
	}
	if len(targets) == 0 {
		return nil, ErrNoChannels
	}
	now := dsp.now()
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	// 记录实际投递目标，便于 GET /v1/messages/{id} 解释「发到了哪」。
	msg.Targets = targets

	deliveries := make([]*model.Delivery, 0, len(targets))
	for _, name := range targets {
		if _, ok := dsp.senders[name]; !ok {
			return nil, fmt.Errorf("未知渠道 %q（可用: %s）", name, strings.Join(available, ", "))
		}
		deliveries = append(deliveries, &model.Delivery{
			ID:            model.NewID("dlv_"),
			MessageID:     msg.ID,
			Channel:       name,
			Status:        model.DeliveryPending,
			MaxAttempts:   dsp.cfg.MaxAttempts,
			NextAttemptAt: &now,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	}

	// 先落库再唤醒 worker：进程在两步之间崩溃也不会丢消息。
	if err := dsp.store.Enqueue(ctx, msg, deliveries); err != nil {
		return nil, err
	}
	dsp.Wake()
	return deliveries, nil
}

// Run 启动 worker 并阻塞到 ctx 被取消。启动时会恢复上次进程遗留的在途投递。
func (dsp *Dispatcher) Run(ctx context.Context) {
	if n, err := dsp.store.RequeueInFlight(ctx); err != nil {
		dsp.log.Error("恢复在途投递失败", "error", err)
	} else if n > 0 {
		dsp.log.Info("已恢复上次未完成的投递", "count", n)
	}

	var wg sync.WaitGroup
	for i := 0; i < dsp.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dsp.worker(ctx)
		}()
	}
	dsp.log.Info("投递 worker 已启动", "workers", dsp.cfg.Workers)
	wg.Wait()
	dsp.log.Info("投递 worker 已退出")
}

func (dsp *Dispatcher) worker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := dsp.store.ClaimDue(ctx, 1, dsp.cfg.Lease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			dsp.log.Error("认领投递失败", "error", err)
			if !dsp.wait(ctx, dsp.cfg.PollInterval) {
				return
			}
			continue
		}
		if len(claimed) == 0 {
			if !dsp.wait(ctx, dsp.cfg.PollInterval) {
				return
			}
			continue
		}
		for _, dlv := range claimed {
			dsp.execute(ctx, dlv)
		}
	}
}

func (dsp *Dispatcher) execute(ctx context.Context, dlv *model.Delivery) {
	msg, err := dsp.store.GetMessage(ctx, dlv.MessageID)
	if err != nil {
		dsp.fail(dlv, fmt.Errorf("读取消息失败: %w", err))
		return
	}
	sender, ok := dsp.senders[dlv.Channel]
	if !ok {
		dsp.fail(dlv, fmt.Errorf("渠道 %q 未启用", dlv.Channel))
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, dsp.cfg.RequestTimeout)
	err = sender.Send(sendCtx, msg)
	cancel()

	if err == nil {
		if perr := dsp.persist(func(pctx context.Context) error {
			return dsp.store.MarkSucceeded(pctx, dlv.ID)
		}); perr != nil {
			dsp.log.Error("记录投递成功失败", "delivery_id", dlv.ID, "error", perr)
		}
		dsp.log.Info("投递成功",
			"message_id", dlv.MessageID,
			"channel", dlv.Channel,
			"attempts", dlv.Attempts,
		)
		return
	}

	if channel.IsPermanent(err) || dlv.Attempts >= dsp.cfg.MaxAttempts {
		dsp.fail(dlv, err)
		return
	}

	backoff := Backoff(dlv.Attempts, dsp.cfg.BaseBackoff, dsp.cfg.MaxBackoff)
	next := dsp.now().Add(backoff)
	if perr := dsp.persist(func(pctx context.Context) error {
		return dsp.store.Reschedule(pctx, dlv.ID, next, err.Error())
	}); perr != nil {
		dsp.log.Error("重新排队投递失败", "delivery_id", dlv.ID, "error", perr)
	}
	dsp.log.Warn("投递失败，稍后重试",
		"message_id", dlv.MessageID,
		"channel", dlv.Channel,
		"attempt", dlv.Attempts,
		"max_attempts", dlv.MaxAttempts,
		"retry_at", next.Format(time.RFC3339),
		"error", err,
	)
}

func (dsp *Dispatcher) fail(dlv *model.Delivery, cause error) {
	if perr := dsp.persist(func(pctx context.Context) error {
		return dsp.store.MarkFailed(pctx, dlv.ID, cause.Error())
	}); perr != nil {
		dsp.log.Error("记录投递失败失败", "delivery_id", dlv.ID, "error", perr)
	}
	dsp.log.Error("投递最终失败",
		"message_id", dlv.MessageID,
		"channel", dlv.Channel,
		"attempts", dlv.Attempts,
		"error", cause,
	)
}

// persist 用独立于请求上下文的短超时上下文写库，避免停机时丢掉状态。
func (dsp *Dispatcher) persist(fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return fn(ctx)
}

func (dsp *Dispatcher) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-dsp.wake:
		return true
	case <-timer.C:
		return true
	}
}

// Backoff 计算第 attempt 次失败后的等待时间：base * 2^(attempt-1)，封顶 max。
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 {
		return 0
	}
	delay := base
	if max <= 0 {
		max = base
	}
	for i := 1; i < attempt; i++ {
		if delay >= max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}
