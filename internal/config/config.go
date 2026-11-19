// Package config 负责加载 YAML 配置、展开环境变量并校验取值。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是服务完整配置。
type Config struct {
	Server   Server   `yaml:"server"`
	Auth     Auth     `yaml:"auth"`
	Database Database `yaml:"database"`
	Dispatch Dispatch `yaml:"dispatch"`
	Channels Channels `yaml:"channels"`

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
		},
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
	c.Server.Addr = strings.TrimSpace(c.Server.Addr)
	c.Database.Path = strings.TrimSpace(c.Database.Path)
	c.Channels.Bark.ServerURL = strings.TrimRight(strings.TrimSpace(c.Channels.Bark.ServerURL), "/")
	c.Channels.Bark.DeviceKey = strings.TrimSpace(c.Channels.Bark.DeviceKey)
	c.Channels.Bark.Group = strings.TrimSpace(c.Channels.Bark.Group)
	c.Channels.Bark.Sound = strings.TrimSpace(c.Channels.Bark.Sound)
	c.Channels.DingTalk.Webhook = strings.TrimSpace(c.Channels.DingTalk.Webhook)
	c.Channels.DingTalk.Secret = strings.TrimSpace(c.Channels.DingTalk.Secret)

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
	if !c.Channels.Bark.Enabled && !c.Channels.DingTalk.Enabled {
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
	return nil
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dev-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
