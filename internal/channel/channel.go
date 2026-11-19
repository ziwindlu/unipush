// Package channel 定义渠道适配器契约与错误分类。
package channel

import (
	"context"
	"errors"
	"sort"

	"github.com/ziwindlu/unipush/internal/model"
)

// Sender 是所有渠道适配器必须实现的接口。
//
// Send 返回 nil 表示该渠道投递成功；返回普通 error 表示可重试失败
// （网络错误、5xx、限流等）；返回 Permanent 包装的错误表示重试也不会成功
// （鉴权失败、参数非法等），投递 worker 会直接标记失败。
type Sender interface {
	// Name 是渠道名，用于配置、targets 与投递记录，例如 "bark"。
	Name() string
	// Send 把一条消息投递到该渠道。
	Send(ctx context.Context, msg *model.Message) error
}

// Set 是渠道名到适配器的映射。
type Set map[string]Sender

// Known 返回当前版本支持的全部渠道名（无论本次运行是否启用）。
// 路由配置里引用这个集合之外的渠道名会在启动时报错。
func Known() []string { return []string{"bark", "dingtalk"} }

// Names 返回排好序的渠道名，保证投递顺序和接口输出稳定。
func (s Set) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type permanentError struct {
	err error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent 标记一个不可重试的错误。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent 报告错误是否被标记为不可重试。
func IsPermanent(err error) bool {
	var target *permanentError
	return errors.As(err, &target)
}
