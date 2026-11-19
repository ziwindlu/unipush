// Package model 定义平台内部共享的消息与投递数据结构。
package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// 消息级别。路由与渠道展示都基于这三个值。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// ValidLevel 报告 level 是否为受支持的级别。
func ValidLevel(level string) bool {
	switch level {
	case LevelInfo, LevelWarn, LevelError:
		return true
	default:
		return false
	}
}

// Route 是一条消息被路由到哪些渠道的记录，用于回答「为什么发到了这里」。
type Route struct {
	// Mode 是命中来源：explicit（调用方显式指定）/ rule（命名规则）/ default（默认目标）。
	Mode string `json:"mode"`
	// Rule 是命中的规则名；显式指定时为空，默认目标为 "default"。
	Rule string `json:"rule,omitempty"`
	// Groups 是命中的渠道组（按配置顺序）。
	Groups []string `json:"groups,omitempty"`
	// Channels 是最终投递的渠道（已排序去重）。
	Channels []string `json:"channels"`
}

// Message 是一条归一化后的待投递消息。
type Message struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Level     string         `json:"level"`
	Source    string         `json:"source"`
	Targets   []string       `json:"targets,omitempty"`
	DedupeKey string         `json:"dedupe_key,omitempty"`
	Link      string         `json:"link,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
	// Route 是路由解析结果；旧数据或未入队的消息可能为空。
	Route *Route `json:"route,omitempty"`
	// MergedCount 是同一 dedupe_key 在去重窗口内被合并掉的次数。
	MergedCount int `json:"merged_count,omitempty"`
	// AggregatedInto 是这条 info 消息被汇总进哪条 digest 消息的 ID。
	AggregatedInto string `json:"aggregated_into,omitempty"`
	// DigestCount 是这条 digest 消息汇总了多少条原始消息。
	DigestCount int       `json:"digest_count,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// EnqueueStatus 是消息入队的结果类型，会作为 POST /v1/messages 的 status 返回。
type EnqueueStatus string

const (
	// EnqueueQueued 表示消息已落库并创建了投递记录。
	EnqueueQueued EnqueueStatus = "queued"
	// EnqueueDeduped 表示命中去重窗口，没有产生新的投递，只累加了合并次数。
	EnqueueDeduped EnqueueStatus = "deduped"
	// EnqueueAggregated 表示 info 消息进入聚合窗口，等 digest 生成后统一投递。
	EnqueueAggregated EnqueueStatus = "aggregated"
)

// EnqueueResult 是一次入队的结果。
//
// 去重命中时 MessageID 是原消息的 ID，Merged 是累计合并次数；
// 聚合时 MessageID 是这条消息自己的 ID（稍后 aggregated_into 会指向 digest），
// 两种情况都不会有新的 Deliveries。
type EnqueueResult struct {
	MessageID  string
	Status     EnqueueStatus
	Merged     int
	Route      *Route
	Deliveries []*Delivery
}

// DeliveryStatus 是单条渠道投递的状态。
type DeliveryStatus string

const (
	// DeliveryPending 表示等待投递（首次或重试前的等待）。
	DeliveryPending DeliveryStatus = "pending"
	// DeliverySending 表示 worker 已认领，正在发送；进程崩溃后会按租约重新入队。
	DeliverySending DeliveryStatus = "sending"
	// DeliverySucceeded 表示该渠道已成功送达，不会再重发。
	DeliverySucceeded DeliveryStatus = "succeeded"
	// DeliveryFailed 表示重试次数耗尽或遇到不可重试错误，已放弃。
	DeliveryFailed DeliveryStatus = "failed"
)

// Delivery 表示一条消息在某个渠道上的投递记录。
type Delivery struct {
	ID          string         `json:"id"`
	MessageID   string         `json:"message_id"`
	Channel     string         `json:"channel"`
	Status      DeliveryStatus `json:"status"`
	Attempts    int            `json:"attempts"`
	MaxAttempts int            `json:"max_attempts"`
	// Rule 是这条投递的路由来源（规则名 / default / 空表示调用方显式指定）。
	Rule string `json:"rule,omitempty"`
	// Groups 是把该渠道带进来的渠道组，fan-out 时可能不止一个。
	Groups        []string   `json:"groups,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	SucceededAt   *time.Time `json:"succeeded_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// NewID 生成一个随机 ID，带前缀便于日志辨认（msg_ / dlv_）。
func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 不可用时退化为时间戳，宁可重复也不阻塞投递。
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

// NormalizeTargets 去空格、转小写、去重并保序。
func NormalizeTargets(targets []string) []string {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// DeliveryCounts 是一条消息各状态投递记录的计数，用来汇总对外状态。
type DeliveryCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// Add 把一条投递记录计入计数；sending 与 pending 都算未完成。
func (c *DeliveryCounts) Add(status DeliveryStatus) {
	c.Total++
	switch status {
	case DeliveryPending, DeliverySending:
		c.Pending++
	case DeliverySucceeded:
		c.Succeeded++
	case DeliveryFailed:
		c.Failed++
	}
}

// Status 把投递计数汇总成对外的消息状态：
// queued / aggregated（还没有投递）/ in_progress / succeeded / partial / failed。
func (c DeliveryCounts) Status(aggregated bool) string {
	switch {
	case c.Total == 0 && aggregated:
		return "aggregated"
	case c.Total == 0:
		return "queued"
	case c.Pending > 0:
		return "in_progress"
	case c.Succeeded > 0 && c.Failed > 0:
		return "partial"
	case c.Failed > 0:
		return "failed"
	default:
		return "succeeded"
	}
}

// ValidStatus 报告消息状态是否是可查询的取值（供 GET /v1/messages 过滤）。
func ValidStatus(status string) bool {
	switch status {
	case "queued", "aggregated", "in_progress", "succeeded", "partial", "failed":
		return true
	default:
		return false
	}
}
