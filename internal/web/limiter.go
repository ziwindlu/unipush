package web

import (
	"sync"
	"time"
)

// loginLimiter 按「来源 IP + 用户名」限制失败次数，防止在线爆破。
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	max     int
	window  time.Duration
	now     func() time.Time
}

type limiterEntry struct {
	failures int
	resetAt  time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		entries: make(map[string]*limiterEntry),
		max:     max,
		window:  window,
		now:     time.Now,
	}
}

// Allow 报告当前是否还能尝试登录。
func (l *loginLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok {
		return true
	}
	if l.now().After(entry.resetAt) {
		delete(l.entries, key)
		return true
	}
	return entry.failures < l.max
}

// Fail 记录一次失败。
func (l *loginLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.entries[key]
	if !ok || now.After(entry.resetAt) {
		entry = &limiterEntry{resetAt: now.Add(l.window)}
		l.entries[key] = entry
	}
	entry.failures++
	if len(l.entries) > 1000 {
		for k, v := range l.entries {
			if now.After(v.resetAt) {
				delete(l.entries, k)
			}
		}
	}
}

// Reset 在登录成功后清除计数。
func (l *loginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
