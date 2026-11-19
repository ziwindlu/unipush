// Package config 负责加载 YAML 配置、展开环境变量并校验取值。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ziwindlu/unipush/internal/routing"
)

// Config 是服务完整配置。
type Config struct {
	Server      Server         `yaml:"server"`
	Auth        Auth           `yaml:"auth"`
	Database    Database       `yaml:"database"`
	Dispatch    Dispatch       `yaml:"dispatch"`
	Channels    Channels       `yaml:"channels"`
	Routing     routing.Config `yaml:"routing"`
	Web         Web            `yaml:"web"`
	Dedupe      Dedupe         `yaml:"dedupe"`
	Aggregation Aggregation    `yaml:"aggregation"`
	Log         Log            `yaml:"log"`

	// Warnings 记录加载过程中被降级处理的配置（例如渠道凭据缺失）。
	Warnings []string `yaml:"-"`
	// EphemeralToken 表示没有配置鉴权 token，本次运行使用了随机临时 token。
	EphemeralToken bool `yaml:"-"`
}

// Server 是 HTTP 服务配置。
type Server struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
}

// Auth 是调用方鉴权配置。
type Auth struct {
	Tokens []string `yaml:"tokens"`
}

// Web 是 Web 控制台配置。
type Web struct {
	// Enabled 关闭后只提供 /v1 API 与 /healthz，不注册 Web 控制台路由。
	Enabled bool `yaml:"enabled"`
	// AdminUser / AdminPassword 只在数据库里还没有任何用户时用于创建初始管理员；
	// 密码留空时会随机生成并打印到启动日志一次。
	AdminUser     string        `yaml:"admin_user"`
	AdminPassword string        `yaml:"admin_password"`
	SessionTTL    time.Duration `yaml:"session_ttl"`
}

// Database 是 SQLite 配置。
type Database struct {
	Path string `yaml:"path"`
}

// Dispatch 是投递 worker 配置。
type Dispatch struct {
	Workers        int           `yaml:"workers"`
	PollInterval   time.Duration `yaml:"poll_interval"`
	Lease          time.Duration `yaml:"lease"`
	MaxAttempts    int           `yaml:"max_attempts"`
	BaseBackoff    time.Duration `yaml:"base_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// Channels 是渠道配置集合。
type Channels struct {
	Bark     Bark     `yaml:"bark"`
	DingTalk DingTalk `yaml:"dingtalk"`
	Feishu   Feishu   `yaml:"feishu"`
	WeCom    WeCom    `yaml:"wecom"`
	Email    Email    `yaml:"email"`
	Webhook  Webhook  `yaml:"webhook"`
}

// Feishu 是飞书自定义机器人配置。
type Feishu struct {
	Enabled bool   `yaml:"enabled"`
	Webhook string `yaml:"webhook"`
	// Secret 是飞书机器人的加签密钥；为空表示未开启加签。
	Secret string `yaml:"secret"`
	// MsgType 是消息形态：text（默认）或 card（带标题的卡片）。
	MsgType string `yaml:"msg_type"`
}

// WeCom 是企业微信群机器人配置。
type WeCom struct {
	Enabled bool   `yaml:"enabled"`
	Webhook string `yaml:"webhook"`
	// MsgType 是消息形态：markdown（默认）或 text。
	MsgType string `yaml:"msg_type"`
}

// Email 是 SMTP 邮件渠道配置。
type Email struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	// Username / Password 只从环境变量注入，不入库。
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
	// TLS 是加密方式：starttls（默认）/ ssl / none。
	TLS           string `yaml:"tls"`
	SubjectPrefix string `yaml:"subject_prefix"`
}

// Webhook 是通用 HTTP webhook 渠道配置。
type Webhook struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	Method  string `yaml:"method"`
	// Headers 是请求头；支持 ${ENV_VAR} 展开。
	Headers map[string]string `yaml:"headers"`
	// ContentType 默认 application/json; charset=utf-8。
	ContentType string `yaml:"content_type"`
	// BodyTemplate 是 Go text/template 模板，可用 {{.Title}} / {{.JSON}} 等字段；
	// 留空时直接发送消息的 JSON 表示。
	BodyTemplate string `yaml:"body_template"`
}

// Dedupe 是去重配置。
type Dedupe struct {
	// Enabled 关闭后 dedupe_key 只做记录，不再合并。
	Enabled bool `yaml:"enabled"`
	// Window 是去重时间窗：窗口内同一 dedupe_key 只投递一次。
	Window time.Duration `yaml:"window"`
}

// Aggregation 是 info 级消息聚合配置。
type Aggregation struct {
	Enabled bool `yaml:"enabled"`
	// Window 是聚合窗口：第一条消息进桶后等待这么久再发 digest。
	Window time.Duration `yaml:"window"`
	// MaxItems 是单个 digest 最多汇总多少条；到量立即发送。
	MaxItems int `yaml:"max_items"`
}

// Log 是日志配置。部署到容器或日志采集时切成 json，一行一条便于检索。
type Log struct {
	// Format 是输出格式：text（默认，人读）/ json（一行一条，采集用）。
	Format string `yaml:"format"`
	// Level 是最低输出级别：debug / info（默认）/ warn / error。
	Level string `yaml:"level"`
}

// Bark 是 Bark 渠道配置。
type Bark struct {
	Enabled   bool   `yaml:"enabled"`
	ServerURL string `yaml:"server_url"`
	DeviceKey string `yaml:"device_key"`
	Group     string `yaml:"group"`
	Sound     string `yaml:"sound"`
}

// DingTalk 是钉钉机器人配置。
type DingTalk struct {
	Enabled bool   `yaml:"enabled"`
	Webhook string `yaml:"webhook"`
	Secret  string `yaml:"secret"`
	// Keyword 是机器人安全设置里的自定义关键词；非空时正文自动拼成「关键词: 内容」。
	Keyword string `yaml:"keyword"`
}

// Default 返回内置默认值。
func Default() *Config {
	return &Config{
		Server: Server{
			Addr:            "127.0.0.1:8080",
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    15 * time.Second,
			IdleTimeout:     60 * time.Second,
			ShutdownTimeout: 10 * time.Second,
			MaxBodyBytes:    1 << 20,
		},
		Database: Database{Path: "./data/unipush.db"},
		Dispatch: Dispatch{
			Workers:        4,
			PollInterval:   time.Second,
			Lease:          60 * time.Second,
			MaxAttempts:    8,
			BaseBackoff:    2 * time.Second,
			MaxBackoff:     5 * time.Minute,
			RequestTimeout: 15 * time.Second,
		},
		Channels: Channels{
			Bark:     Bark{Enabled: true, ServerURL: "https://api.day.app", Group: "unipush"},
			DingTalk: DingTalk{Enabled: true},
			Feishu:   Feishu{Enabled: true, MsgType: "text"},
			WeCom:    WeCom{Enabled: true, MsgType: "markdown"},
			Email: Email{
				Enabled:       true,
				Port:          587,
				TLS:           "starttls",
				SubjectPrefix: "[unipush]",
			},
			Webhook: Webhook{
				Enabled:     true,
				Method:      http.MethodPost,
				ContentType: "application/json; charset=utf-8",
			},
		},
		Web:         Web{Enabled: true, AdminUser: "admin", SessionTTL: 7 * 24 * time.Hour},
		Dedupe:      Dedupe{Enabled: true, Window: 5 * time.Minute},
		Aggregation: Aggregation{Enabled: true, Window: time.Minute, MaxItems: 50},
		Log:         Log{Format: "text", Level: "info"},
	}
}

// Load 读取配置，展开 ${ENV_VAR}，补齐默认值并做校验。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	// YAML 里的 ${VAR} 从环境变量注入密钥；未设置的变量展开为空串，由校验兜底。
	expanded := os.Expand(string(raw), func(key string) string { return os.Getenv(key) })

	cfg := Default()
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadDotEnv 读取 KEY=VALUE 形式的 .env 文件，只填充尚未设置的环境变量。
// 文件不存在时返回 0, nil，方便开发时直接 go run。
func LoadDotEnv(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	loaded := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return loaded, fmt.Errorf("设置环境变量 %s: %w", key, err)
		}
		loaded++
	}
	return loaded, nil
}

func (c *Config) normalize() {
	// 容器里不改配置文件也能覆盖监听地址和数据库路径（Docker 默认 0.0.0.0 + /data）。
	if v := strings.TrimSpace(os.Getenv("UNIPUSH_ADDR")); v != "" {
		c.Server.Addr = v
	}
	if v := strings.TrimSpace(os.Getenv("UNIPUSH_DB_PATH")); v != "" {
		c.Database.Path = v
	}
	c.Server.Addr = strings.TrimSpace(c.Server.Addr)
	c.Database.Path = strings.TrimSpace(c.Database.Path)
	c.Channels.Bark.ServerURL = strings.TrimRight(strings.TrimSpace(c.Channels.Bark.ServerURL), "/")
	c.Channels.Bark.DeviceKey = strings.TrimSpace(c.Channels.Bark.DeviceKey)
	c.Channels.Bark.Group = strings.TrimSpace(c.Channels.Bark.Group)
	c.Channels.Bark.Sound = strings.TrimSpace(c.Channels.Bark.Sound)
	c.Channels.DingTalk.Webhook = strings.TrimSpace(c.Channels.DingTalk.Webhook)
	c.Channels.DingTalk.Secret = strings.TrimSpace(c.Channels.DingTalk.Secret)
	c.Channels.DingTalk.Keyword = strings.TrimSpace(c.Channels.DingTalk.Keyword)

	c.Channels.Feishu.Webhook = strings.TrimSpace(c.Channels.Feishu.Webhook)
	c.Channels.Feishu.Secret = strings.TrimSpace(c.Channels.Feishu.Secret)
	c.Channels.Feishu.MsgType = strings.ToLower(strings.TrimSpace(c.Channels.Feishu.MsgType))
	if c.Channels.Feishu.MsgType == "" {
		c.Channels.Feishu.MsgType = "text"
	}

	c.Channels.WeCom.Webhook = strings.TrimSpace(c.Channels.WeCom.Webhook)
	c.Channels.WeCom.MsgType = strings.ToLower(strings.TrimSpace(c.Channels.WeCom.MsgType))
	if c.Channels.WeCom.MsgType == "" {
		c.Channels.WeCom.MsgType = "markdown"
	}

	c.Channels.Email.Host = strings.TrimSpace(c.Channels.Email.Host)
	c.Channels.Email.Username = strings.TrimSpace(c.Channels.Email.Username)
	c.Channels.Email.Password = strings.TrimSpace(c.Channels.Email.Password)
	c.Channels.Email.From = strings.TrimSpace(c.Channels.Email.From)
	c.Channels.Email.SubjectPrefix = strings.TrimSpace(c.Channels.Email.SubjectPrefix)
	c.Channels.Email.TLS = strings.ToLower(strings.TrimSpace(c.Channels.Email.TLS))
	if c.Channels.Email.TLS == "" {
		c.Channels.Email.TLS = "starttls"
	}
	if c.Channels.Email.Port == 0 {
		c.Channels.Email.Port = 587
	}
	recipients := make([]string, 0, len(c.Channels.Email.To))
	for _, to := range c.Channels.Email.To {
		if to = strings.TrimSpace(to); to != "" {
			recipients = append(recipients, to)
		}
	}
	c.Channels.Email.To = recipients

	c.Channels.Webhook.URL = strings.TrimSpace(c.Channels.Webhook.URL)
	c.Channels.Webhook.Method = strings.ToUpper(strings.TrimSpace(c.Channels.Webhook.Method))
	if c.Channels.Webhook.Method == "" {
		c.Channels.Webhook.Method = http.MethodPost
	}
	c.Channels.Webhook.ContentType = strings.TrimSpace(c.Channels.Webhook.ContentType)
	if c.Channels.Webhook.ContentType == "" {
		c.Channels.Webhook.ContentType = "application/json; charset=utf-8"
	}
	if len(c.Channels.Webhook.Headers) > 0 {
		headers := make(map[string]string, len(c.Channels.Webhook.Headers))
		for k, v := range c.Channels.Webhook.Headers {
			if k = strings.TrimSpace(k); k != "" {
				headers[k] = strings.TrimSpace(v)
			}
		}
		c.Channels.Webhook.Headers = headers
	}

	// 日志格式与级别也支持环境变量覆盖，容器里不用改配置文件。
	if v := strings.TrimSpace(os.Getenv("UNIPUSH_LOG_FORMAT")); v != "" {
		c.Log.Format = v
	}
	if v := strings.TrimSpace(os.Getenv("UNIPUSH_LOG_LEVEL")); v != "" {
		c.Log.Level = v
	}
	c.Log.Format = strings.ToLower(strings.TrimSpace(c.Log.Format))
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}

	// 管理员初始账号优先取环境变量，避免密码里的特殊字符参与 YAML 解析。
	if v := strings.TrimSpace(os.Getenv("UNIPUSH_ADMIN_USER")); v != "" {
		c.Web.AdminUser = v
	}
	if v := strings.TrimSpace(os.Getenv("UNIPUSH_ADMIN_PASSWORD")); v != "" {
		c.Web.AdminPassword = v
	}
	c.Web.AdminUser = strings.TrimSpace(c.Web.AdminUser)
	if c.Web.AdminUser == "" {
		c.Web.AdminUser = "admin"
	}
	c.Web.AdminPassword = strings.TrimSpace(c.Web.AdminPassword)
	if c.Web.SessionTTL <= 0 {
		c.Web.SessionTTL = 7 * 24 * time.Hour
	}

	tokens := make([]string, 0, len(c.Auth.Tokens))
	for _, t := range c.Auth.Tokens {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	c.Auth.Tokens = tokens
	if len(tokens) == 0 {
		// 没有配置 token 时生成随机临时 token，保证 go run 能直接启动；
		// 启动日志会打印它，仅供本机调试。
		c.Auth.Tokens = []string{randomToken()}
		c.EphemeralToken = true
		c.Warnings = append(c.Warnings,
			"未配置 UNIPUSH_AUTH_TOKEN，本次运行使用随机临时 token（重启后会变化，生产环境必须配置固定 token）")
	}

	if c.Channels.Bark.Enabled && c.Channels.Bark.DeviceKey == "" {
		c.Channels.Bark.Enabled = false
		c.Warnings = append(c.Warnings, "BARK_DEVICE_KEY 未设置，bark 渠道已禁用")
	}
	if c.Channels.DingTalk.Enabled && c.Channels.DingTalk.Webhook == "" {
		c.Channels.DingTalk.Enabled = false
		c.Warnings = append(c.Warnings, "DINGTALK_WEBHOOK 未设置，dingtalk 渠道已禁用")
	}
	if c.Channels.Feishu.Enabled && c.Channels.Feishu.Webhook == "" {
		c.Channels.Feishu.Enabled = false
		c.Warnings = append(c.Warnings, "FEISHU_WEBHOOK 未设置，feishu 渠道已禁用")
	}
	if c.Channels.WeCom.Enabled && c.Channels.WeCom.Webhook == "" {
		c.Channels.WeCom.Enabled = false
		c.Warnings = append(c.Warnings, "WECOM_WEBHOOK 未设置，wecom 渠道已禁用")
	}
	if c.Channels.Email.Enabled && (c.Channels.Email.Host == "" || c.Channels.Email.From == "" || len(c.Channels.Email.To) == 0) {
		c.Channels.Email.Enabled = false
		c.Warnings = append(c.Warnings, "email 渠道缺少 host / from / to，已禁用")
	}
	if c.Channels.Webhook.Enabled && c.Channels.Webhook.URL == "" {
		c.Channels.Webhook.Enabled = false
		c.Warnings = append(c.Warnings, "WEBHOOK_URL 未设置，webhook 渠道已禁用")
	}
	if !c.Channels.Bark.Enabled && !c.Channels.DingTalk.Enabled &&
		!c.Channels.Feishu.Enabled && !c.Channels.WeCom.Enabled &&
		!c.Channels.Email.Enabled && !c.Channels.Webhook.Enabled {
		c.Warnings = append(c.Warnings, "没有启用任何渠道：未指定 targets 的消息会被拒绝，直到配置好至少一个渠道")
	}
}

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database.path 不能为空")
	}
	if c.Dispatch.Workers < 1 {
		return fmt.Errorf("dispatch.workers 必须 >= 1")
	}
	if c.Dispatch.PollInterval <= 0 {
		return fmt.Errorf("dispatch.poll_interval 必须大于 0")
	}
	if c.Dispatch.Lease <= 0 {
		return fmt.Errorf("dispatch.lease 必须大于 0")
	}
	if c.Dispatch.MaxAttempts < 1 {
		return fmt.Errorf("dispatch.max_attempts 必须 >= 1")
	}
	if c.Dispatch.BaseBackoff <= 0 {
		return fmt.Errorf("dispatch.base_backoff 必须大于 0")
	}
	if c.Dispatch.MaxBackoff < c.Dispatch.BaseBackoff {
		return fmt.Errorf("dispatch.max_backoff 不能小于 dispatch.base_backoff")
	}
	if c.Dispatch.RequestTimeout <= 0 {
		return fmt.Errorf("dispatch.request_timeout 必须大于 0")
	}
	if c.Server.MaxBodyBytes <= 0 {
		return fmt.Errorf("server.max_body_bytes 必须大于 0")
	}
	switch c.Channels.Feishu.MsgType {
	case "text", "card":
	default:
		return fmt.Errorf("channels.feishu.msg_type 只能是 text 或 card，当前是 %q", c.Channels.Feishu.MsgType)
	}
	switch c.Channels.WeCom.MsgType {
	case "markdown", "text":
	default:
		return fmt.Errorf("channels.wecom.msg_type 只能是 markdown 或 text，当前是 %q", c.Channels.WeCom.MsgType)
	}
	switch c.Channels.Email.TLS {
	case "starttls", "ssl", "none":
	default:
		return fmt.Errorf("channels.email.tls 只能是 starttls / ssl / none，当前是 %q", c.Channels.Email.TLS)
	}
	if c.Channels.Email.Port < 1 || c.Channels.Email.Port > 65535 {
		return fmt.Errorf("channels.email.port 必须在 1-65535 之间")
	}
	if c.Dedupe.Enabled && c.Dedupe.Window <= 0 {
		return fmt.Errorf("dedupe.window 必须大于 0")
	}
	if c.Aggregation.Enabled {
		if c.Aggregation.Window <= 0 {
			return fmt.Errorf("aggregation.window 必须大于 0")
		}
		if c.Aggregation.MaxItems < 1 {
			return fmt.Errorf("aggregation.max_items 必须 >= 1")
		}
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format 只能是 text 或 json，当前是 %q", c.Log.Format)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level 只能是 debug / info / warn / error，当前是 %q", c.Log.Level)
	}
	if c.Web.Enabled {
		if c.Web.SessionTTL <= 0 {
			return fmt.Errorf("web.session_ttl 必须大于 0")
		}
		if len(c.Web.AdminPassword) > 0 && len(c.Web.AdminPassword) < 8 {
			return fmt.Errorf("web.admin_password 至少 8 个字符")
		}
	}
	return nil
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dev-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
