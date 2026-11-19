package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置: %v", err)
	}
	return path
}

func TestLoadExpandsEnvAndAppliesDefaults(t *testing.T) {
	t.Setenv("TEST_UNIPUSH_TOKEN", "token-from-env")
	t.Setenv("TEST_BARK_KEY", "")
	t.Setenv("TEST_DINGTALK_WEBHOOK", "")

	path := writeConfig(t, `
auth:
  tokens:
    - ${TEST_UNIPUSH_TOKEN}
channels:
  bark:
    device_key: ${TEST_BARK_KEY}
  dingtalk:
    webhook: ${TEST_DINGTALK_WEBHOOK}
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Auth.Tokens) != 1 || cfg.Auth.Tokens[0] != "token-from-env" {
		t.Fatalf("token 未从环境变量展开: %#v", cfg.Auth.Tokens)
	}
	if cfg.Server.Addr != "127.0.0.1:8080" {
		t.Errorf("addr 默认值错误: %s", cfg.Server.Addr)
	}
	if cfg.Database.Path != "./data/unipush.db" {
		t.Errorf("database.path 默认值错误: %s", cfg.Database.Path)
	}
	if cfg.Dispatch.Workers != 4 || cfg.Dispatch.MaxAttempts != 8 {
		t.Errorf("dispatch 默认值错误: %+v", cfg.Dispatch)
	}
	if cfg.Dispatch.BaseBackoff != 2*time.Second || cfg.Dispatch.MaxBackoff != 5*time.Minute {
		t.Errorf("退避默认值错误: base=%s max=%s", cfg.Dispatch.BaseBackoff, cfg.Dispatch.MaxBackoff)
	}
	// 凭据缺失的渠道应被禁用并给出告警，而不是让服务启动失败。
	if cfg.Channels.Bark.Enabled {
		t.Error("bark device key 为空时应禁用 bark")
	}
	if cfg.Channels.DingTalk.Enabled {
		t.Error("dingtalk webhook 为空时应禁用 dingtalk")
	}
	if len(cfg.Warnings) < 2 {
		t.Errorf("期望至少 2 条告警，实际: %v", cfg.Warnings)
	}
}

func TestLoadGeneratesEphemeralTokenWhenMissing(t *testing.T) {
	os.Unsetenv("TEST_UNIPUSH_EMPTY_TOKEN")
	path := writeConfig(t, `
auth:
  tokens:
    - ${TEST_UNIPUSH_EMPTY_TOKEN}
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EphemeralToken {
		t.Fatal("未配置 token 时应标记使用临时 token")
	}
	if len(cfg.Auth.Tokens) != 1 || len(cfg.Auth.Tokens[0]) < 16 {
		t.Fatalf("临时 token 不合法: %#v", cfg.Auth.Tokens)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"workers 为 0": `dispatch:
  workers: 0
`,
		"负的退避": `dispatch:
  base_backoff: -1s
`,
		"max_backoff 小于 base": `dispatch:
  base_backoff: 10s
  max_backoff: 1s
`,
		"空 addr": `server:
  addr: ""
`,
		"空数据库路径": `database:
  path: ""
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, body)); err == nil {
				t.Fatalf("期望校验失败")
			}
		})
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	if _, err := config.Load(writeConfig(t, "server: [not: valid")); err == nil {
		t.Fatal("非法 YAML 期望报错")
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := strings.Join([]string{
		"# 注释",
		"TEST_DOTENV_NEW=\"quoted value\"",
		"export TEST_DOTENV_EXPORT=exported",
		"TEST_DOTENV_EXISTING=from-file",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 .env: %v", err)
	}
	t.Setenv("TEST_DOTENV_EXISTING", "from-env")
	os.Unsetenv("TEST_DOTENV_NEW")
	os.Unsetenv("TEST_DOTENV_EXPORT")

	n, err := config.LoadDotEnv(path)
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if n != 2 {
		t.Errorf("加载数量 = %d, 期望 2（已存在的环境变量不覆盖）", n)
	}
	if got := os.Getenv("TEST_DOTENV_NEW"); got != "quoted value" {
		t.Errorf("TEST_DOTENV_NEW = %q", got)
	}
	if got := os.Getenv("TEST_DOTENV_EXPORT"); got != "exported" {
		t.Errorf("TEST_DOTENV_EXPORT = %q", got)
	}
	if got := os.Getenv("TEST_DOTENV_EXISTING"); got != "from-env" {
		t.Errorf("已存在的环境变量被覆盖: %q", got)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	n, err := config.LoadDotEnv(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil || n != 0 {
		t.Fatalf("缺失 .env 应静默返回，实际 n=%d err=%v", n, err)
	}
}

// TestLoadDisablesChannelsWithoutCredentials 保证新增渠道凭据缺失时只是禁用并告警，
// 不会让服务起不来；同时检查 M3 的默认值。
func TestLoadDisablesChannelsWithoutCredentials(t *testing.T) {
	for _, key := range []string{
		"TEST_FEISHU_WEBHOOK", "TEST_WECOM_WEBHOOK", "TEST_WEBHOOK_URL",
		"TEST_SMTP_FROM", "TEST_SMTP_USER",
	} {
		t.Setenv(key, "")
	}
	cfg, err := config.Load(writeConfig(t, `
auth:
  tokens: [test-token]
channels:
  feishu:
    webhook: ${TEST_FEISHU_WEBHOOK}
  wecom:
    webhook: ${TEST_WECOM_WEBHOOK}
  email:
    from: ${TEST_SMTP_FROM}
  webhook:
    url: ${TEST_WEBHOOK_URL}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Channels.Feishu.Enabled || cfg.Channels.WeCom.Enabled ||
		cfg.Channels.Email.Enabled || cfg.Channels.Webhook.Enabled {
		t.Errorf("凭据缺失的新渠道应被禁用: %+v", cfg.Channels)
	}
	if cfg.Channels.Feishu.MsgType != "text" || cfg.Channels.WeCom.MsgType != "markdown" {
		t.Errorf("新渠道消息形态默认值错误: feishu=%q wecom=%q",
			cfg.Channels.Feishu.MsgType, cfg.Channels.WeCom.MsgType)
	}
	if cfg.Channels.Email.TLS != "starttls" || cfg.Channels.Email.Port != 587 {
		t.Errorf("email 默认值错误: tls=%q port=%d", cfg.Channels.Email.TLS, cfg.Channels.Email.Port)
	}
	if cfg.Channels.Webhook.Method != "POST" {
		t.Errorf("webhook 方法默认值错误: %q", cfg.Channels.Webhook.Method)
	}
	if !cfg.Dedupe.Enabled || cfg.Dedupe.Window != 5*time.Minute {
		t.Errorf("去重默认值错误: %+v", cfg.Dedupe)
	}
	if !cfg.Aggregation.Enabled || cfg.Aggregation.Window != time.Minute || cfg.Aggregation.MaxItems != 50 {
		t.Errorf("聚合默认值错误: %+v", cfg.Aggregation)
	}
}

// TestLoadNormalizesNewChannels 检查配置项的大小写/空格归一化与启用判断。
func TestLoadNormalizesNewChannels(t *testing.T) {
	t.Setenv("TEST_FEISHU_WEBHOOK", "https://open.feishu.cn/open-apis/bot/v2/hook/abc")
	t.Setenv("TEST_WECOM_WEBHOOK", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc")
	t.Setenv("TEST_WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("TEST_SMTP_FROM", "bot@example.com")

	cfg, err := config.Load(writeConfig(t, `
auth:
  tokens: [test-token]
channels:
  feishu:
    webhook: ${TEST_FEISHU_WEBHOOK}
    msg_type: " Card "
  wecom:
    webhook: ${TEST_WECOM_WEBHOOK}
  email:
    host: smtp.example.com
    from: ${TEST_SMTP_FROM}
    to: [" me@example.com ", ""]
  webhook:
    url: ${TEST_WEBHOOK_URL}
    method: put
    headers:
      " X-Token ": " secret "
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Channels.Feishu.Enabled || cfg.Channels.Feishu.MsgType != "card" {
		t.Errorf("feishu 归一化错误: %+v", cfg.Channels.Feishu)
	}
	if !cfg.Channels.WeCom.Enabled || !cfg.Channels.Email.Enabled || !cfg.Channels.Webhook.Enabled {
		t.Errorf("四个新渠道都应启用: %+v", cfg.Channels)
	}
	if len(cfg.Channels.Email.To) != 1 || cfg.Channels.Email.To[0] != "me@example.com" {
		t.Errorf("收件人应去空格并丢弃空项: %#v", cfg.Channels.Email.To)
	}
	if cfg.Channels.Webhook.Method != "PUT" {
		t.Errorf("webhook 方法应转大写: %q", cfg.Channels.Webhook.Method)
	}
	if got := cfg.Channels.Webhook.Headers["X-Token"]; got != "secret" {
		t.Errorf("请求头应去空格: %#v", cfg.Channels.Webhook.Headers)
	}
}

// TestLoadRejectsInvalidM3Settings 保证错误的渠道设置与去重/聚合参数在启动时暴露。
func TestLoadRejectsInvalidM3Settings(t *testing.T) {
	cases := map[string]string{
		"feishu msg_type 非法": `channels:
  feishu:
    webhook: https://example.com/hook
    msg_type: markdown
`,
		"wecom msg_type 非法": `channels:
  wecom:
    webhook: https://example.com/hook
    msg_type: card
`,
		"email tls 非法": `channels:
  email:
    host: smtp.example.com
    from: a@example.com
    to: [b@example.com]
    tls: tls
`,
		"email port 越界": `channels:
  email:
    host: smtp.example.com
    port: 70000
    from: a@example.com
    to: [b@example.com]
`,
		"dedupe window 为 0": `dedupe:
  enabled: true
  window: 0s
`,
		"aggregation max_items 为 0": `aggregation:
  enabled: true
  window: 1m
  max_items: 0
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, body)); err == nil {
				t.Fatalf("期望校验失败")
			}
		})
	}
}

// TestLoadLogDefaultsAndOverrides 覆盖 M4：日志格式/级别既能在配置文件里写，也能用环境变量覆盖。
func TestLoadWebDefaultsAndAdminEnv(t *testing.T) {
	t.Setenv("UNIPUSH_ADMIN_USER", "ops")
	// 密码里的冒号和井号不能参与 YAML 解析，必须直接从环境变量读取。
	t.Setenv("UNIPUSH_ADMIN_PASSWORD", "p@ss: word#1")
	path := writeConfig(t, "auth:\n  tokens: [test-token]\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Web.Enabled {
		t.Error("Web 控制台默认应启用")
	}
	if cfg.Web.AdminUser != "ops" || cfg.Web.AdminPassword != "p@ss: word#1" {
		t.Fatalf("管理员环境变量未生效: %+v", cfg.Web)
	}
	if cfg.Web.SessionTTL != 168*time.Hour {
		t.Errorf("session_ttl 默认值 = %v", cfg.Web.SessionTTL)
	}
}

func TestLoadLogDefaultsAndOverrides(t *testing.T) {
	path := writeConfig(t, `
auth:
  tokens: [test-token]
channels:
  bark: {enabled: false}
  dingtalk: {enabled: false}
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Format != "text" || cfg.Log.Level != "info" {
		t.Fatalf("日志默认值错误: %+v", cfg.Log)
	}

	t.Setenv("UNIPUSH_LOG_FORMAT", "JSON")
	t.Setenv("UNIPUSH_LOG_LEVEL", "debug")
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Format != "json" || cfg.Log.Level != "debug" {
		t.Fatalf("环境变量没有覆盖日志配置: %+v", cfg.Log)
	}
}

// TestLoadRejectsInvalidLogSettings 保证写错格式/级别在启动时就报错，而不是静默回退。
func TestLoadRejectsInvalidLogSettings(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"format", "log:\n  format: xml\n", "log.format"},
		{"level", "log:\n  level: verbose\n", "log.level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "auth:\n  tokens: [test-token]\n"+tc.body)
			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望 %q 校验错误，实际: %v", tc.want, err)
			}
		})
	}
}

// TestLoadEnvOverridesAddrAndDatabase 让同一份 config.yaml 能在容器里换成 0.0.0.0 和 /data。
func TestLoadEnvOverridesAddrAndDatabase(t *testing.T) {
	t.Setenv("UNIPUSH_ADDR", "0.0.0.0:9090")
	t.Setenv("UNIPUSH_DB_PATH", "/data/unipush.db")
	path := writeConfig(t, "auth:\n  tokens: [test-token]\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != "0.0.0.0:9090" {
		t.Errorf("UNIPUSH_ADDR 未生效: %s", cfg.Server.Addr)
	}
	if cfg.Database.Path != "/data/unipush.db" {
		t.Errorf("UNIPUSH_DB_PATH 未生效: %s", cfg.Database.Path)
	}
}
