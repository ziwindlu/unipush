// Command push 是 unipush 的命令行客户端：给脚本和 cron 一个不用拼 curl 的发送入口。
//
// 用法：
//
//	push send --title "备份完成" --level info --source nas-monitor --body "3 个文件"
//
// 服务地址与 token 默认取 UNIPUSH_URL / UNIPUSH_AUTH_TOKEN，也会自动加载当前目录的 .env；
// 命令行参数优先于环境变量。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/config"
	"github.com/ziwindlu/unipush/internal/model"
)

var version = "dev"

const (
	defaultURL     = "http://127.0.0.1:8080"
	defaultTimeout = 10 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "send":
		return runSend(args[1:], stdin, stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "push", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `push 是 unipush 的命令行客户端。

用法:
  push send [参数]     发送一条消息
  push version         打印版本
  push help            打印本帮助

send 参数:
  --title string        标题（与 --body 至少一个）
  --body string         正文；传 "-" 从标准输入读取
  --level string        级别：info / warn / error（默认 info）
  --source string       来源标识，路由规则按它匹配（例如 nas-monitor）
  --targets strings     显式投递目标，逗号分隔：渠道组名或渠道名（如 phone,team）
  --dedupe-key string   去重键，窗口内相同 key 只投递一次
  --link string         http(s) 链接，Bark 可点击打开
  --url string          unipush 地址（默认 $UNIPUSH_URL 或 `+defaultURL+`）
  --token string        Bearer Token（默认 $UNIPUSH_AUTH_TOKEN）
  --env-file string     启动时加载的环境变量文件（默认 ".env"，不存在则忽略）
  --timeout duration    请求超时（默认 `+defaultTimeout.String()+`）
  --json               打印服务端返回的原始 JSON
  --quiet, -q           只打印消息 ID，便于脚本取值

退出码: 0 成功；1 请求或服务端错误；2 用法或配置错误。
`)
}

// stringSlice 支持 --targets phone,team 和重复传参两种写法。
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// sendRequest 是 POST /v1/messages 的请求体。
type sendRequest struct {
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	Level     string   `json:"level,omitempty"`
	Source    string   `json:"source,omitempty"`
	Targets   []string `json:"targets,omitempty"`
	DedupeKey string   `json:"dedupe_key,omitempty"`
	Link      string   `json:"link,omitempty"`
}

// sendResponse 是成功响应里 CLI 展示需要的字段。
type sendResponse struct {
	ID         string        `json:"id"`
	Status     string        `json:"status"`
	Merged     int           `json:"merged"`
	Route      *model.Route  `json:"route"`
	Deliveries []cliDelivery `json:"deliveries"`
}

type cliDelivery struct {
	Channel string   `json:"channel"`
	Status  string   `json:"status"`
	Groups  []string `json:"groups"`
}

// errorResponse 与服务端错误结构一致。
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"error"`
}

func runSend(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push send", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		title     string
		body      string
		level     string
		source    string
		dedupeKey string
		link      string
		baseURL   string
		token     string
		envFile   string
		timeout   time.Duration
		asJSON    bool
		quiet     bool
		targets   stringSlice
	)
	fs.StringVar(&title, "title", "", "标题")
	fs.StringVar(&body, "body", "", "正文；- 表示从标准输入读取")
	fs.StringVar(&level, "level", "", "级别：info / warn / error")
	fs.StringVar(&source, "source", "", "来源标识")
	fs.Var(&targets, "targets", "显式投递目标，逗号分隔")
	fs.StringVar(&dedupeKey, "dedupe-key", "", "去重键")
	fs.StringVar(&link, "link", "", "消息链接")
	fs.StringVar(&baseURL, "url", "", "unipush 地址")
	fs.StringVar(&token, "token", "", "Bearer Token")
	fs.StringVar(&envFile, "env-file", ".env", "环境变量文件")
	fs.DurationVar(&timeout, "timeout", defaultTimeout, "请求超时")
	fs.BoolVar(&asJSON, "json", false, "打印原始 JSON 响应")
	fs.BoolVar(&quiet, "quiet", false, "只打印消息 ID")
	fs.BoolVar(&quiet, "q", false, "只打印消息 ID")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "send 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	// .env 与服务端共用一份，脚本里不用重复导出 token。
	if envFile != "" {
		if _, err := config.LoadDotEnv(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "警告: 加载 %s 失败: %v\n", envFile, err)
		}
	}

	if body == "-" {
		raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			fmt.Fprintf(stderr, "读取标准输入失败: %v\n", err)
			return 1
		}
		body = string(raw)
	}

	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		level = model.LevelInfo
	}
	if !model.ValidLevel(level) {
		fmt.Fprintf(stderr, "level 必须是 info / warn / error 之一，当前是 %q\n", level)
		return 2
	}
	if strings.TrimSpace(title) == "" && strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "title 与 body 不能同时为空")
		return 2
	}

	if token == "" {
		token = strings.TrimSpace(os.Getenv("UNIPUSH_AUTH_TOKEN"))
	}
	if token == "" {
		fmt.Fprintln(stderr, "缺少 token：用 --token 传入，或设置 UNIPUSH_AUTH_TOKEN / 在同目录 .env 里配置")
		return 2
	}
	if err := requestSend(baseURL, token, timeout, &sendRequest{
		Title:     title,
		Body:      body,
		Level:     level,
		Source:    strings.TrimSpace(source),
		Targets:   targets,
		DedupeKey: strings.TrimSpace(dedupeKey),
		Link:      strings.TrimSpace(link),
	}, asJSON, quiet, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// requestSend 发送消息并把结果写到 stdout。baseURL 为空时取环境变量或默认本机地址。
func requestSend(baseURL, token string, timeout time.Duration, payload *sendRequest, asJSON, quiet bool, stdout io.Writer) error {
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("UNIPUSH_URL"))
	}
	if baseURL == "" {
		baseURL = defaultURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 unipush 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr errorResponse
		if err := json.Unmarshal(raw, &apiErr); err == nil && apiErr.Error.Message != "" {
			field := ""
			if apiErr.Error.Field != "" {
				field = " (字段 " + apiErr.Error.Field + ")"
			}
			return fmt.Errorf("投递失败: HTTP %d: %s%s", resp.StatusCode, apiErr.Error.Message, field)
		}
		return fmt.Errorf("投递失败: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if asJSON {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			buf.Write(raw)
		}
		fmt.Fprintln(stdout, strings.TrimRight(buf.String(), "\n"))
		return nil
	}

	var result sendResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if quiet {
		fmt.Fprintln(stdout, result.ID)
		return nil
	}
	printResult(stdout, &result)
	return nil
}

// printResult 输出一行状态 + 一行路由，cron 日志里也能一眼看出投到哪了。
func printResult(w io.Writer, result *sendResponse) {
	switch result.Status {
	case string(model.EnqueueDeduped):
		fmt.Fprintf(w, "%s 已合并（同一 dedupe_key 的第 %d 次，未重复投递）\n", result.ID, result.Merged+1)
	case string(model.EnqueueAggregated):
		fmt.Fprintf(w, "%s 已进入聚合窗口，稍后随 digest 投递\n", result.ID)
	default:
		fmt.Fprintf(w, "%s 已入队\n", result.ID)
	}
	if result.Route != nil {
		var target string
		switch result.Route.Mode {
		case "rule":
			target = "规则 " + result.Route.Rule
		case "explicit":
			target = "调用方显式 targets"
		default:
			target = "默认目标"
		}
		groups := ""
		if len(result.Route.Groups) > 0 {
			groups = " → 组 " + strings.Join(result.Route.Groups, ",")
		}
		fmt.Fprintf(w, "路由：%s%s → 渠道 %s\n", target, groups, strings.Join(result.Route.Channels, ","))
	}
	if len(result.Deliveries) > 0 {
		channels := make([]string, 0, len(result.Deliveries))
		for _, d := range result.Deliveries {
			channels = append(channels, d.Channel)
		}
		fmt.Fprintf(w, "投递记录：%s\n", strings.Join(channels, ","))
	}
}
