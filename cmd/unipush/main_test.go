package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置: %v", err)
	}
	return path
}

// TestServeFailsOnUnknownRoutingGroup 覆盖 M2 验收 3：
// 规则引用了不存在的渠道组时，服务在开始监听之前就失败，并指出配置位置。
func TestServeFailsOnUnknownRoutingGroup(t *testing.T) {
	path := writeConfig(t, `
auth:
  tokens: [test-token]
channels:
  bark:
    enabled: false
  dingtalk:
    enabled: false
routing:
  groups:
    phone: [bark]
  default: [phone]
  rules:
    - name: nas-alerts
      sources: ["nas-*"]
      groups: [ghost]
`)
	err := serve(path, discardLogger())
	if err == nil {
		t.Fatal("规则引用不存在的渠道组时，启动应直接失败")
	}
	for _, want := range []string{"routing.rules[0].groups[0]", "未定义的渠道组", "ghost"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("启动错误应包含 %q，实际: %s", want, err.Error())
		}
	}
}

// TestServeFailsOnUnknownChannelInGroup 保证组里写了未知渠道也是在启动时报错，而不是静默跳过。
func TestServeFailsOnUnknownChannelInGroup(t *testing.T) {
	path := writeConfig(t, `
auth:
  tokens: [test-token]
channels:
  bark:
    enabled: false
  dingtalk:
    enabled: false
routing:
  groups:
    phone: [bark, telegram]
  default: [phone]
`)
	err := serve(path, discardLogger())
	if err == nil {
		t.Fatal("组里引用未知渠道时，启动应直接失败")
	}
	for _, want := range []string{"routing.groups.phone.channels[1]", "未知渠道", "telegram"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("启动错误应包含 %q，实际: %s", want, err.Error())
		}
	}
}

func TestBuildRouterCompilesValidConfig(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
auth:
  tokens: [test-token]
channels:
  bark:
    enabled: false
  dingtalk:
    enabled: false
routing:
  groups:
    phone: [bark]
    team: [dingtalk]
    all: [bark, dingtalk]
  default: [all]
  rules:
    - name: nas-alerts
      sources: ["nas-*"]
      levels: [error]
      groups: [phone]
`))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	router, err := buildRouter(cfg, channel.Set{})
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}
	if got := strings.Join(router.Groups(), ","); got != "all,phone,team" {
		t.Errorf("Groups = %q", got)
	}
	if !router.Configured() {
		t.Error("配置了 routing 段，Configured() 应为 true")
	}
	// 两个渠道都没启用：应给出「组里没有已启用渠道」的告警，而不是让消息静默消失。
	warnings := strings.Join(router.Warnings(), "\n")
	for _, group := range []string{"phone", "team", "all"} {
		if !strings.Contains(warnings, group) {
			t.Errorf("应提示渠道组 %s 没有已启用渠道，实际: %s", group, warnings)
		}
	}
}

func TestBuildRouterWithoutRoutingSection(t *testing.T) {
	cfg := config.Default()
	router, err := buildRouter(cfg, channel.Set{"bark": nil})
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}
	if router.Configured() {
		t.Error("未配置 routing 段时 Configured() 应为 false")
	}
}

// TestSampleConfigLoads 保证仓库里的 config.yaml 样例始终能被加载并编译出路由，
// 避免示例配置与代码里的字段名脱节。
func TestSampleConfigLoads(t *testing.T) {
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("加载 config.yaml 样例失败: %v", err)
	}
	if !cfg.Dedupe.Enabled || cfg.Dedupe.Window <= 0 {
		t.Errorf("样例配置应开启去重: %+v", cfg.Dedupe)
	}
	if !cfg.Aggregation.Enabled || cfg.Aggregation.Window <= 0 || cfg.Aggregation.MaxItems < 1 {
		t.Errorf("样例配置应开启聚合: %+v", cfg.Aggregation)
	}
	// 样例里没有真实凭据，渠道会被自动禁用；路由仍应能编译（组里没有已启用渠道只是告警）。
	if _, err := buildRouter(cfg, channel.Set{}); err != nil {
		t.Fatalf("样例配置的路由编译失败: %v", err)
	}
}
