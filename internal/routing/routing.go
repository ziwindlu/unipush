// Package routing 决定一条消息最终投递到哪些渠道。
//
// 优先级从高到低：
//  1. 调用方在 targets 里显式指定的渠道组 / 渠道名；
//  2. 按 source + level 匹配到的第一条规则（规则按配置顺序求值，先写更具体的）；
//  3. 默认投递目标 routing.default。
//
// 渠道组与规则在启动时校验：引用未定义的组、未知渠道、非法级别都会让服务起不来，
// 而不是在投递时静默丢弃消息。
package routing

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/ziwindlu/unipush/internal/model"
)

// 路由命中的来源，会写进消息的 route.mode。
const (
	// ModeExplicit 表示调用方在 targets 里显式指定了投递目标。
	ModeExplicit = "explicit"
	// ModeRule 表示命中了某条命名规则。
	ModeRule = "rule"
	// ModeDefault 表示没有命中规则，使用了默认投递目标。
	ModeDefault = "default"
)

// Config 对应 config.yaml 的 routing 段。
type Config struct {
	// Groups 是渠道组：组名 → 渠道名列表，例如 phone: [bark]。
	Groups map[string][]string `yaml:"groups"`
	// Default 是没命中任何规则时的投递目标（渠道组列表）。
	Default []string `yaml:"default"`
	// Rules 按顺序求值，第一条命中的生效。
	Rules []Rule `yaml:"rules"`
}

// Rule 是一条路由规则：source / level 同时满足后，投递到 Groups 里的渠道组。
// Sources 支持 shell 通配（path.Match），Levels 是精确级别名；留空表示不限。
type Rule struct {
	Name    string   `yaml:"name"`
	Sources []string `yaml:"sources"`
	Levels  []string `yaml:"levels"`
	Groups  []string `yaml:"groups"`
}

// Options 是路由需要的渠道信息。
type Options struct {
	// SupportedChannels 是本版本支持的渠道名，用于校验配置里引用的渠道是否存在。
	SupportedChannels []string
	// EnabledChannels 是本次运行实际启用的渠道；未启用的渠道不参与投递。
	EnabledChannels []string
}

// Decision 是一次路由解析的结果。
type Decision struct {
	Mode   string
	Rule   string
	Groups []string
	// Channels 是最终要投递的渠道，已排序去重。
	Channels []string
	// ChannelGroups 记录每个渠道是被哪些组带进来的，用于解释「为什么发到了这里」。
	ChannelGroups map[string][]string
}

// Route 返回写入消息与投递记录的路由结果。
func (d *Decision) Route() *model.Route {
	return &model.Route{
		Mode:     d.Mode,
		Rule:     d.Rule,
		Groups:   d.Groups,
		Channels: d.Channels,
	}
}

// UnknownTargetError 表示 targets 里出现了既不是渠道组、也不是已启用渠道的名字。
type UnknownTargetError struct {
	Target   string
	Groups   []string
	Channels []string
}

func (e *UnknownTargetError) Error() string {
	return fmt.Sprintf("未知的投递目标 %q（可用渠道组: %s；可用渠道: %s）",
		e.Target, joinOrNone(e.Groups), joinOrNone(e.Channels))
}

// NoRouteError 表示这条消息没有任何可投递的渠道：
// 没有命中规则且没有默认目标，或命中的组里没有已启用的渠道。
type NoRouteError struct {
	Reason string
	Rule   string
	Groups []string
}

func (e *NoRouteError) Error() string { return e.Reason }

// Router 把配置编译成可复用的路由表。
type Router struct {
	enabled       map[string]struct{}
	groups        map[string][]string
	rules         []compiledRule
	defaultGroups []string
	warnings      []string
	configured    bool
}

type compiledRule struct {
	name    string
	sources []string
	levels  []string
	groups  []string
}

// New 校验并编译路由配置。配置里出现未定义的渠道组、未知渠道、非法级别时返回错误，
// 错误信息带上 config.yaml 里的位置（例如 routing.rules[0].groups[1]），便于直接定位。
func New(cfg Config, opts Options) (*Router, error) {
	r := &Router{
		enabled: toSet(opts.EnabledChannels),
		groups:  map[string][]string{},
	}
	supported := toSet(opts.SupportedChannels)
	r.configured = len(cfg.Groups) > 0 || len(cfg.Rules) > 0 || len(cfg.Default) > 0
	if !r.configured {
		return r, nil
	}

	if err := r.loadGroups(cfg.Groups, supported); err != nil {
		return nil, err
	}
	groupNames := r.Groups()

	seenRules := make(map[string]int, len(cfg.Rules))
	for i, rule := range cfg.Rules {
		compiled, err := compileRule(i, rule, r.groups, groupNames)
		if err != nil {
			return nil, err
		}
		if prev, dup := seenRules[compiled.name]; dup {
			return nil, fmt.Errorf("routing.rules[%d].name: 规则名 %q 与 routing.rules[%d] 重复", i, compiled.name, prev)
		}
		seenRules[compiled.name] = i
		r.rules = append(r.rules, compiled)
	}

	defaults := normalizeList(cfg.Default)
	for i, group := range defaults {
		if _, ok := r.groups[group]; !ok {
			return nil, fmt.Errorf("routing.default[%d]: 未定义的渠道组 %q（已定义: %s）", i, group, joinOrNone(groupNames))
		}
	}
	r.defaultGroups = defaults

	if len(r.rules) == 0 && len(r.defaultGroups) == 0 {
		r.warnings = append(r.warnings,
			"routing 既没有 rules 也没有 default：不带 targets 的消息会被拒绝（503）")
	} else if len(r.defaultGroups) == 0 {
		r.warnings = append(r.warnings,
			"未配置 routing.default：没命中任何规则的消息会被拒绝（503）")
	}

	return r, nil
}

func (r *Router) loadGroups(groups map[string][]string, supported map[string]struct{}) error {
	rawNames := make([]string, 0, len(groups))
	for name := range groups {
		rawNames = append(rawNames, name)
	}
	sort.Strings(rawNames)

	for _, raw := range rawNames {
		name := normalizeName(raw)
		if name == "" {
			return fmt.Errorf("routing.groups.%s: 渠道组名不能为空", raw)
		}
		if _, clash := supported[name]; clash {
			return fmt.Errorf("routing.groups.%s: 组名与渠道名 %q 相同，targets 无法区分，请换一个组名", raw, name)
		}
		if _, dup := r.groups[name]; dup {
			return fmt.Errorf("routing.groups.%s: 渠道组 %q 重复定义（组名忽略大小写与首尾空白）", raw, name)
		}

		channels := normalizeList(groups[raw])
		if len(channels) == 0 {
			return fmt.Errorf("routing.groups.%s.channels: 至少配置一个渠道", raw)
		}
		for i, ch := range channels {
			if _, ok := supported[ch]; !ok {
				return fmt.Errorf("routing.groups.%s.channels[%d]: 未知渠道 %q（当前支持: %s）",
					raw, i, ch, joinOrNone(sortedKeys(supported)))
			}
		}
		r.groups[name] = channels

		if !r.anyEnabled(channels) {
			r.warnings = append(r.warnings, fmt.Sprintf(
				"渠道组 %s 里没有已启用的渠道（%s），命中它的消息会因无可用渠道而失败",
				name, strings.Join(channels, ", ")))
		}
	}
	return nil
}

func compileRule(index int, rule Rule, groups map[string][]string, groupNames []string) (compiledRule, error) {
	loc := fmt.Sprintf("routing.rules[%d]", index)

	name := strings.TrimSpace(rule.Name)
	if name == "" {
		return compiledRule{}, fmt.Errorf("%s.name: 规则必须有名字（会写进投递记录，便于排查）", loc)
	}

	sources := normalizeList(rule.Sources)
	for i, pattern := range sources {
		if _, err := path.Match(pattern, "probe"); err != nil {
			return compiledRule{}, fmt.Errorf("%s.sources[%d]: 非法的通配模式 %q", loc, i, pattern)
		}
	}

	levels := normalizeList(rule.Levels)
	for i, level := range levels {
		if !model.ValidLevel(level) {
			return compiledRule{}, fmt.Errorf("%s.levels[%d]: 未知级别 %q（可选: info, warn, error）", loc, i, level)
		}
	}

	if len(sources) == 0 && len(levels) == 0 {
		return compiledRule{}, fmt.Errorf(
			"%s: 规则没有任何匹配条件（sources / levels 至少一个），会吞掉所有消息；通用兜底请用 routing.default", loc)
	}

	targets := normalizeList(rule.Groups)
	if len(targets) == 0 {
		return compiledRule{}, fmt.Errorf("%s.groups: 至少配置一个渠道组", loc)
	}
	for i, group := range targets {
		if _, ok := groups[group]; !ok {
			return compiledRule{}, fmt.Errorf("%s.groups[%d]: 未定义的渠道组 %q（已定义: %s）",
				loc, i, group, joinOrNone(groupNames))
		}
	}

	return compiledRule{name: name, sources: sources, levels: levels, groups: targets}, nil
}

// Groups 返回已定义的渠道组名（已排序）。
func (r *Router) Groups() []string {
	names := make([]string, 0, len(r.groups))
	for name := range r.groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Warnings 返回不影响启动、但值得提醒的配置问题。
func (r *Router) Warnings() []string { return r.warnings }

// Configured 报告是否配置了 routing 段。
func (r *Router) Configured() bool { return r.configured }

// Resolve 按 targets → 规则 → 默认目标的顺序解析投递渠道。
func (r *Router) Resolve(targets []string, source, level string) (*Decision, error) {
	if targets = normalizeList(targets); len(targets) > 0 {
		return r.resolveTargets(targets)
	}
	if !r.configured {
		return r.allChannels()
	}

	source = normalizeName(source)
	level = normalizeName(level)
	for _, rule := range r.rules {
		if !rule.matches(source, level) {
			continue
		}
		channels, channelGroups := r.expand(rule.groups)
		if len(channels) == 0 {
			return nil, &NoRouteError{
				Reason: fmt.Sprintf("规则 %q 命中 source=%q level=%q，但目标组 %s 没有可用渠道（已启用: %s）",
					rule.name, source, level, strings.Join(rule.groups, ", "), strings.Join(r.enabledNames(), ", ")),
				Rule:   rule.name,
				Groups: append([]string(nil), rule.groups...),
			}
		}
		return &Decision{
			Mode:          ModeRule,
			Rule:          rule.name,
			Groups:        append([]string(nil), rule.groups...),
			Channels:      channels,
			ChannelGroups: channelGroups,
		}, nil
	}

	if len(r.defaultGroups) == 0 {
		return nil, &NoRouteError{
			Reason: fmt.Sprintf("没有匹配 source=%q level=%q 的路由规则，且未配置 routing.default", source, level),
		}
	}
	channels, channelGroups := r.expand(r.defaultGroups)
	if len(channels) == 0 {
		return nil, &NoRouteError{
			Reason: fmt.Sprintf("默认投递目标 %s 没有可用渠道（已启用: %s）",
				strings.Join(r.defaultGroups, ", "), strings.Join(r.enabledNames(), ", ")),
			Groups: append([]string(nil), r.defaultGroups...),
		}
	}
	return &Decision{
		Mode:          ModeDefault,
		Rule:          "default",
		Groups:        append([]string(nil), r.defaultGroups...),
		Channels:      channels,
		ChannelGroups: channelGroups,
	}, nil
}

func (r *Router) resolveTargets(targets []string) (*Decision, error) {
	seen := make(map[string]struct{}, len(targets))
	channels := make([]string, 0, len(targets))
	channelGroups := map[string][]string{}
	groups := make([]string, 0, len(targets))

	for _, target := range targets {
		if members, ok := r.groups[target]; ok {
			groups = append(groups, target)
			for _, ch := range members {
				if _, enabled := r.enabled[ch]; !enabled {
					continue
				}
				if _, dup := seen[ch]; !dup {
					seen[ch] = struct{}{}
					channels = append(channels, ch)
				}
				channelGroups[ch] = appendUnique(channelGroups[ch], target)
			}
			continue
		}
		if _, enabled := r.enabled[target]; enabled {
			if _, dup := seen[target]; !dup {
				seen[target] = struct{}{}
				channels = append(channels, target)
			}
			continue
		}
		return nil, &UnknownTargetError{
			Target:   target,
			Groups:   r.Groups(),
			Channels: r.enabledNames(),
		}
	}

	if len(channels) == 0 {
		return nil, &NoRouteError{
			Reason: fmt.Sprintf("targets %s 没有可用渠道（已启用: %s）",
				strings.Join(targets, ", "), strings.Join(r.enabledNames(), ", ")),
			Groups: groups,
		}
	}

	sort.Strings(channels)
	return &Decision{
		Mode:          ModeExplicit,
		Groups:        groups,
		Channels:      channels,
		ChannelGroups: channelGroups,
	}, nil
}

func (r *Router) allChannels() (*Decision, error) {
	channels := r.enabledNames()
	if len(channels) == 0 {
		return nil, &NoRouteError{Reason: "没有启用任何渠道，无法投递"}
	}
	return &Decision{
		Mode:          ModeDefault,
		Rule:          "default",
		Channels:      channels,
		ChannelGroups: map[string][]string{},
	}, nil
}

// expand 把渠道组列表展开成渠道集合；未启用的渠道被跳过，组信息保留用于排查。
func (r *Router) expand(groups []string) ([]string, map[string][]string) {
	seen := make(map[string]struct{})
	channels := make([]string, 0, len(groups))
	channelGroups := make(map[string][]string, len(groups))
	for _, group := range groups {
		for _, ch := range r.groups[group] {
			if _, enabled := r.enabled[ch]; !enabled {
				continue
			}
			if _, dup := seen[ch]; !dup {
				seen[ch] = struct{}{}
				channels = append(channels, ch)
			}
			channelGroups[ch] = appendUnique(channelGroups[ch], group)
		}
	}
	sort.Strings(channels)
	return channels, channelGroups
}

func (r *Router) anyEnabled(channels []string) bool {
	for _, ch := range channels {
		if _, ok := r.enabled[ch]; ok {
			return true
		}
	}
	return false
}

func (r *Router) enabledNames() []string { return sortedKeys(r.enabled) }

func (rule compiledRule) matches(source, level string) bool {
	if len(rule.sources) > 0 {
		matched := false
		for _, pattern := range rule.sources {
			if pattern == source {
				matched = true
				break
			}
			if ok, err := path.Match(pattern, source); err == nil && ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(rule.levels) > 0 {
		found := false
		for _, candidate := range rule.levels {
			if candidate == level {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func normalizeList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizeName(value)
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func toSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = normalizeName(value); value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "（无）"
	}
	return strings.Join(values, ", ")
}
