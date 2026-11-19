package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"
)

func parsePages() (map[string]*template.Template, error) {
	pages := []string{"dashboard", "messages", "message", "send", "tokens", "users", "account", "error"}
	out := make(map[string]*template.Template, len(pages)+1)
	for _, page := range pages {
		tpl, err := template.New("base").Funcs(templateFuncs()).ParseFS(assets,
			"templates/base.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("web: 解析模板 %s: %w", page, err)
		}
		out[page] = tpl
	}
	login, err := template.New("login").Funcs(templateFuncs()).ParseFS(assets, "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("web: 解析登录模板: %w", err)
	}
	out["login"] = login
	return out, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtTime":        fmtTime,
		"fmtTimePtr":     fmtTimePtr,
		"statusLabel":    statusLabel,
		"statusClass":    statusClass,
		"deliveryLabel":  deliveryLabel,
		"join":           strings.Join,
		"truncate":       truncate,
		"jsonPretty":     jsonPretty,
		"defaultText":    defaultText,
		"levelLabel":     levelLabel,
		"containsString": containsString,
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func fmtTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func statusLabel(status string) string {
	switch status {
	case "queued":
		return "排队中"
	case "aggregated":
		return "已聚合"
	case "in_progress":
		return "投递中"
	case "succeeded":
		return "已送达"
	case "partial":
		return "部分失败"
	case "failed":
		return "失败"
	default:
		return defaultText(status, "未知")
	}
}

func statusClass(status string) string {
	switch status {
	case "queued", "aggregated", "in_progress":
		return "chip chip-warn"
	case "succeeded":
		return "chip chip-ok"
	case "partial":
		return "chip chip-warn"
	case "failed":
		return "chip chip-bad"
	default:
		return "chip"
	}
}

func deliveryLabel(status string) string {
	switch status {
	case "pending":
		return "等待"
	case "sending":
		return "发送中"
	case "succeeded":
		return "成功"
	case "failed":
		return "失败"
	default:
		return defaultText(status, "-")
	}
}

func levelLabel(level string) string {
	switch level {
	case "info":
		return "信息"
	case "warn":
		return "警告"
	case "error":
		return "错误"
	default:
		return defaultText(level, "-")
	}
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func jsonPretty(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

func defaultText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
