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
	Route     *Route    `json:"route,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
