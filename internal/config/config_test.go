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
