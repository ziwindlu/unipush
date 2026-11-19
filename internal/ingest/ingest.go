// Package ingest 把外部请求归一化成 model.Message，
// 供 HTTP 接口与 Web 控制台共用同一套校验规则。
package ingest

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/model"
)

// Request 是创建消息的原始输入。
type Request struct {
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Level     string         `json:"level"`
	Source    string         `json:"source"`
	Targets   []string       `json:"targets"`
	DedupeKey string         `json:"dedupe_key"`
	Link      string         `json:"link"`
	Extra     map[string]any `json:"extra"`
}

// Error 是可以直接返回给调用方的校验错误。
type Error struct {
	Status  int
	Code    string
	Message string
	Field   string
}

func (e *Error) Error() string { return e.Message }

func invalid(message, field string) *Error {
	return &Error{Status: 400, Code: "invalid_request", Message: message, Field: field}
}

// Build 校验输入并生成消息；channels / groups 是当前可用的投递目标。
func Build(req *Request, channels, groups []string) (*model.Message, *Error) {
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	if title == "" && body == "" {
		return nil, invalid("title 与 body 不能同时为空", "title")
	}
	if utf8.RuneCountInString(title) > 200 {
		return nil, invalid("title 不能超过 200 个字符", "title")
	}
	if utf8.RuneCountInString(body) > 8000 {
		return nil, invalid("body 不能超过 8000 个字符", "body")
	}

	level := strings.ToLower(strings.TrimSpace(req.Level))
	if level == "" {
		level = model.LevelInfo
	}
	if !model.ValidLevel(level) {
		return nil, invalid("level 必须是 info / warn / error 之一", "level")
	}

	source := strings.TrimSpace(req.Source)
	if utf8.RuneCountInString(source) > 64 {
		return nil, invalid("source 不能超过 64 个字符", "source")
	}
	dedupeKey := strings.TrimSpace(req.DedupeKey)
	if utf8.RuneCountInString(dedupeKey) > 256 {
		return nil, invalid("dedupe_key 不能超过 256 个字符", "dedupe_key")
	}

	link := strings.TrimSpace(req.Link)
	if link != "" {
		u, err := url.Parse(link)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, invalid("link 必须是 http(s) 绝对地址", "link")
		}
	}

	targets := model.NormalizeTargets(req.Targets)
	if len(targets) > 0 {
		known := make(map[string]struct{}, len(channels)+len(groups))
		for _, name := range groups {
			known[name] = struct{}{}
		}
		for _, name := range channels {
			known[name] = struct{}{}
		}
		for _, name := range targets {
			if _, ok := known[name]; !ok {
				return nil, invalid(fmt.Sprintf("未知投递目标 %q（可用渠道组: %s；可用渠道: %s）",
					name, JoinNames(groups), JoinNames(channels)), "targets")
			}
		}
	} else if len(channels) == 0 {
		return nil, &Error{
			Status:  503,
			Code:    "no_channels",
			Message: "服务未配置任何可用渠道",
			Field:   "targets",
		}
	}

	return &model.Message{
		ID:        model.NewID("msg_"),
		Title:     title,
		Body:      body,
		Level:     level,
		Source:    source,
		Targets:   targets,
		DedupeKey: dedupeKey,
		Link:      link,
		Extra:     req.Extra,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// JoinNames 用于错误提示里的名称列表。
func JoinNames(names []string) string {
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, ", ")
}
