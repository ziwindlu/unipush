package web

import (
	"sync"
	"time"
)

const flashTTL = 5 * time.Minute

// flashMessage 是跨一次重定向展示的提示；Secret 只展示一次（新建 Token / 初始密码）。
type flashMessage struct {
	Kind        string
	Text        string
	Secret      string
	SecretLabel string
	Expires     time.Time
}

type flashStore struct {
	mu    sync.Mutex
	items map[string]flashMessage
	now   func() time.Time
}

func newFlashStore() *flashStore {
	return &flashStore{items: make(map[string]flashMessage), now: time.Now}
}

func (f *flashStore) Set(key string, msg flashMessage) {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) > 500 {
		for k, v := range f.items {
			if now.After(v.Expires) {
				delete(f.items, k)
			}
		}
	}
	msg.Expires = now.Add(flashTTL)
	f.items[key] = msg
}

func (f *flashStore) Pop(key string) *flashMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, ok := f.items[key]
	if !ok {
		return nil
	}
	delete(f.items, key)
	if f.now().After(msg.Expires) {
		return nil
	}
	return &msg
}
