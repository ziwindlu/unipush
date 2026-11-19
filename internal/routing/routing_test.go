package routing_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/routing"
)

var supported = []string{"bark", "dingtalk"}

// baseConfig 是 M2 的典型配置：手机、群、全渠道三个组，两个来源规则 + 默认目标。
func baseConfig() routing.Config {
	return routing.Config{
		Groups: map[string][]string{
			"phone": {"bark"},
			"team":  {"dingtalk"},
			"all":   {"bark", "dingtalk"},
		},
		Default: []string{"all"},
		Rules: []routing.Rule{
			{Name: "nas-alerts", Sources: []string{"nas-*"}, Levels: []string{"warn", "error"}, Groups: []string{"phone"}},
			{Name: "ci-build", Sources: []string{"ci"}, Levels: []string{"info"}, Groups: []string{"team"}},
		},
	}
}

func newRouter(t *testing.T, cfg routing.Config) *routing.Router {
	t.Helper()
	return newRouterWith(t, cfg, supported)
}

func newRouterWith(t *testing.T, cfg routing.Config, enabled []string) *routing.Router {
	t.Helper()
	router, err := routing.New(cfg, routing.Options{SupportedChannels: supported, EnabledChannels: enabled})
	if err != nil {
		t.Fatalf("routing.New: %v", err)
	}
	return router
}

func assertDecision(t *testing.T, d *routing.Decision, mode, rule string, groups, channels []string) {
	t.Helper()
	if d.Mode != mode {
		t.Errorf("mode = %q, 期望 %q", d.Mode, mode)
	}
	if d.Rule != rule {
		t.Errorf("rule = %q, 期望 %q", d.Rule, rule)
	}
	if strings.Join(d.Groups, ",") != strings.Join(groups, ",") {
		t.Errorf("groups = %v, 期望 %v", d.Groups, groups)
	}
	if strings.Join(d.Channels, ",") != strings.Join(channels, ",") {
		t.Errorf("channels = %v, 期望 %v", d.Channels, channels)
	}
}

func TestResolveBySourceAndLevel(t *testing.T) {
	router := newRouter(t, baseConfig())

	cases := []struct {
		name    string
		source  string
		level   string
		mode    string
		rule    string
		groups  []string
		channel []string
	}{
		{"来源通配命中", "nas-monitor", model.LevelError, routing.ModeRule, "nas-alerts", []string{"phone"}, []string{"bark"}},
		{"来源精确命中", "ci", model.LevelInfo, routing.ModeRule, "ci-build", []string{"team"}, []string{"dingtalk"}},
		{"来源命中但级别不匹配走默认", "ci", model.LevelError, routing.ModeDefault, "default", []string{"all"}, []string{"bark", "dingtalk"}},
		{"没有规则命中走默认", "manual", model.LevelInfo, routing.ModeDefault, "default", []string{"all"}, []string{"bark", "dingtalk"}},
		{"来源大小写不敏感", "NAS-Monitor", model.LevelError, routing.ModeRule, "nas-alerts", []string{"phone"}, []string{"bark"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decision, err := router.Resolve(nil, c.source, c.level)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			assertDecision(t, decision, c.mode, c.rule, c.groups, c.channel)
		})
	}
}

func TestRuleOrderFirstMatchWins(t *testing.T) {
	cfg := routing.Config{
		Groups:  map[string][]string{"phone": {"bark"}, "team": {"dingtalk"}},
		Default: []string{"team"},
		Rules: []routing.Rule{
			{Name: "errors-first", Levels: []string{"error"}, Groups: []string{"team"}},
			{Name: "all-errors", Sources: []string{"*"}, Levels: []string{"error"}, Groups: []string{"phone"}},
		},
	}
	router := newRouter(t, cfg)
	decision, err := router.Resolve(nil, "any", model.LevelError)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeRule, "errors-first", []string{"team"}, []string{"dingtalk"})
}

func TestResolveFanOutToUnionOfGroups(t *testing.T) {
	cfg := routing.Config{
		Groups:  map[string][]string{"phone": {"bark"}, "team": {"dingtalk"}, "all": {"bark", "dingtalk"}},
		Default: []string{"all"},
		Rules: []routing.Rule{
			{Name: "critical", Levels: []string{"error"}, Groups: []string{"phone", "team", "all"}},
		},
	}
	router := newRouter(t, cfg)
	decision, err := router.Resolve(nil, "x", model.LevelError)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// 三个组并集仍是 bark + dingtalk，但每个渠道要记住是哪些组带进来的。
	assertDecision(t, decision, routing.ModeRule, "critical", []string{"phone", "team", "all"}, []string{"bark", "dingtalk"})
	if got := strings.Join(decision.ChannelGroups["bark"], ","); got != "phone,all" {
		t.Errorf("bark 的渠道组 = %q, 期望 phone,all", got)
	}
	if got := strings.Join(decision.ChannelGroups["dingtalk"], ","); got != "team,all" {
		t.Errorf("dingtalk 的渠道组 = %q, 期望 team,all", got)
	}
}

func TestExplicitTargetsOverrideRules(t *testing.T) {
	router := newRouter(t, baseConfig())

	// 来源本来该投 phone，但调用方显式要求 team。
	decision, err := router.Resolve([]string{"TEAM"}, "nas-monitor", model.LevelError)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeExplicit, "", []string{"team"}, []string{"dingtalk"})
	if got := decision.ChannelGroups["dingtalk"]; len(got) != 1 || got[0] != "team" {
		t.Errorf("dingtalk 的渠道组 = %v, 期望 [team]", got)
	}

	// 直接写渠道名仍然可用（M1 兼容）。
	decision, err = router.Resolve([]string{"bark"}, "nas-monitor", model.LevelError)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeExplicit, "", nil, []string{"bark"})
	if len(decision.ChannelGroups["bark"]) != 0 {
		t.Errorf("直接指定渠道时不应有渠道组: %v", decision.ChannelGroups["bark"])
	}

	// 多个组去重后按渠道排序。
	decision, err = router.Resolve([]string{"phone", "all"}, "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeExplicit, "", []string{"phone", "all"}, []string{"bark", "dingtalk"})
}

func TestExplicitUnknownTargetError(t *testing.T) {
	router := newRouter(t, baseConfig())
	_, err := router.Resolve([]string{"telegram"}, "", "")
	var unknown *routing.UnknownTargetError
	if !errors.As(err, &unknown) {
		t.Fatalf("期望 UnknownTargetError，实际 %v", err)
	}
	if unknown.Target != "telegram" {
		t.Errorf("Target = %q", unknown.Target)
	}
	msg := err.Error()
	for _, want := range []string{"telegram", "phone", "team", "all", "bark", "dingtalk"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q，实际: %s", want, msg)
		}
	}
}

func TestNoRouteWhenNoDefaultAndNothingMatched(t *testing.T) {
	cfg := routing.Config{
		Groups: map[string][]string{"team": {"dingtalk"}},
		Rules: []routing.Rule{
			{Name: "ci", Sources: []string{"ci"}, Groups: []string{"team"}},
		},
	}
	router := newRouter(t, cfg)
	_, err := router.Resolve(nil, "other", model.LevelInfo)
	var noRoute *routing.NoRouteError
	if !errors.As(err, &noRoute) {
		t.Fatalf("期望 NoRouteError，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "routing.default") {
		t.Errorf("错误信息应提示未配置 routing.default: %s", err.Error())
	}
}

func TestDisabledChannelsAreSkipped(t *testing.T) {
	// bark 没有凭据被禁用：默认组 all 仍可投 dingtalk，只投 phone 的规则则无路可走。
	router := newRouterWith(t, baseConfig(), []string{"dingtalk"})

	decision, err := router.Resolve(nil, "manual", model.LevelInfo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeDefault, "default", []string{"all"}, []string{"dingtalk"})
	if got := decision.ChannelGroups["dingtalk"]; len(got) != 1 || got[0] != "all" {
		t.Errorf("dingtalk 的渠道组 = %v, 期望 [all]", got)
	}

	if _, err := router.Resolve(nil, "nas-monitor", model.LevelError); !errors.As(err, new(*routing.NoRouteError)) {
		t.Fatalf("只投被禁用渠道的规则应返回 NoRouteError，实际 %v", err)
	}
	if _, err := router.Resolve([]string{"phone"}, "", ""); !errors.As(err, new(*routing.NoRouteError)) {
		t.Fatalf("显式指定被禁用的组应返回 NoRouteError，实际 %v", err)
	}

	warnings := strings.Join(router.Warnings(), "\n")
	if !strings.Contains(warnings, "phone") || !strings.Contains(warnings, "没有已启用") {
		t.Errorf("应提示 phone 组没有已启用渠道，实际: %s", warnings)
	}
}

func TestNoRoutingConfigKeepsM1Behaviour(t *testing.T) {
	router := newRouter(t, routing.Config{})
	if router.Configured() {
		t.Error("空 routing 不应被视为已配置")
	}
	if groups := router.Groups(); len(groups) != 0 {
		t.Errorf("Groups = %v, 期望空", groups)
	}
	decision, err := router.Resolve(nil, "anything", model.LevelWarn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeDefault, "default", nil, []string{"bark", "dingtalk"})

	decision, err = router.Resolve([]string{"bark"}, "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertDecision(t, decision, routing.ModeExplicit, "", nil, []string{"bark"})
}

func TestResolveWithoutEnabledChannels(t *testing.T) {
	router := newRouterWith(t, routing.Config{
		Groups:  map[string][]string{"phone": {"bark"}},
		Default: []string{"phone"},
	}, nil)

	_, err := router.Resolve(nil, "manual", model.LevelInfo)
	var noRoute *routing.NoRouteError
	if !errors.As(err, &noRoute) {
		t.Fatalf("没有任何启用渠道时应返回 NoRouteError，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "没有可用渠道") {
		t.Errorf("错误信息应说明没有可用渠道: %s", err.Error())
	}
	if _, err := router.Resolve([]string{"phone"}, "", ""); !errors.As(err, &noRoute) {
		t.Fatalf("显式指定组时也应返回 NoRouteError，实际 %v", err)
	}
}

func TestConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*routing.Config)
		want   []string
	}{
		{
			name: "规则引用未定义的组",
			mutate: func(c *routing.Config) {
				c.Rules[0].Groups = []string{"ghost"}
			},
			want: []string{"routing.rules[0].groups[0]", "未定义的渠道组", "ghost"},
		},
		{
			name: "默认目标引用未定义的组",
			mutate: func(c *routing.Config) {
				c.Default = []string{"ghost"}
			},
			want: []string{"routing.default[0]", "ghost"},
		},
		{
			name: "组里引用未知渠道",
			mutate: func(c *routing.Config) {
				c.Groups["phone"] = []string{"feishu"}
			},
			want: []string{"routing.groups.phone.channels[0]", "未知渠道", "feishu"},
		},
		{
			name: "组里没有渠道",
			mutate: func(c *routing.Config) {
				c.Groups["phone"] = nil
			},
			want: []string{"routing.groups.phone.channels", "至少配置一个渠道"},
		},
		{
			name: "组名与渠道名冲突",
			mutate: func(c *routing.Config) {
				c.Groups["dingtalk"] = []string{"dingtalk"}
			},
			want: []string{"routing.groups.dingtalk", "组名与渠道名"},
		},
		{
			name: "规则重名",
			mutate: func(c *routing.Config) {
				c.Rules[1].Name = c.Rules[0].Name
			},
			want: []string{"routing.rules[1].name", "重复"},
		},
		{
			name: "规则缺名字",
			mutate: func(c *routing.Config) {
				c.Rules[0].Name = "  "
			},
			want: []string{"routing.rules[0].name", "规则必须有名字"},
		},
		{
			name: "未知级别",
			mutate: func(c *routing.Config) {
				c.Rules[0].Levels = []string{"fatal"}
			},
			want: []string{"routing.rules[0].levels[0]", "fatal"},
		},
		{
			name: "非法通配模式",
			mutate: func(c *routing.Config) {
				c.Rules[0].Sources = []string{"nas-["}
			},
			want: []string{"routing.rules[0].sources[0]", "通配"},
		},
		{
			name: "规则没有匹配条件",
			mutate: func(c *routing.Config) {
				c.Rules[0].Sources = nil
				c.Rules[0].Levels = nil
			},
			want: []string{"routing.rules[0]", "没有任何匹配条件"},
		},
		{
			name: "规则没有目标组",
			mutate: func(c *routing.Config) {
				c.Rules[0].Groups = nil
			},
			want: []string{"routing.rules[0].groups", "至少配置一个渠道组"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig()
			c.mutate(&cfg)
			_, err := routing.New(cfg, routing.Options{SupportedChannels: supported, EnabledChannels: supported})
			if err == nil {
				t.Fatal("期望配置校验失败")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息应包含 %q，实际: %s", want, err.Error())
				}
			}
		})
	}
}

func TestValidConfigNormalizesNames(t *testing.T) {
	cfg := routing.Config{
		Groups: map[string][]string{
			" Phone ": {"BARK"},
			"Team":    {"DingTalk"},
		},
		Default: []string{"Phone"},
		Rules: []routing.Rule{
			{Name: "Ci", Sources: []string{"CI"}, Levels: []string{"INFO"}, Groups: []string{"Team"}},
		},
	}
	router := newRouter(t, cfg)
	if got := strings.Join(router.Groups(), ","); got != "phone,team" {
		t.Errorf("组名未归一化: %q", got)
	}
	decision, err := router.Resolve(nil, "ci", model.LevelInfo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if decision.Mode != routing.ModeRule {
		t.Errorf("mode = %q, 期望 rule", decision.Mode)
	}
	if got := strings.Join(decision.Channels, ","); got != "dingtalk" {
		t.Errorf("channels = %q", got)
	}
}
