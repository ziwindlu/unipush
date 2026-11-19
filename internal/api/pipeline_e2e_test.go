package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/channel"
	"github.com/ziwindlu/unipush/internal/channel/bark"
	"github.com/ziwindlu/unipush/internal/dispatch"
	"github.com/ziwindlu/unipush/internal/store"
)

// TestEndToEndDedupesAndAggregates 覆盖 M3 验收 2 与 3：
// 同一 dedupe_key 连发 10 次只投递 1 次、记录里显示合并了 9 次；
// info 级消息按窗口聚合成一条 digest，error 级消息立即投递。
func TestEndToEndDedupesAndAggregates(t *testing.T) {
	rec := &barkRecorder{}
	barkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		rec.mu.Lock()
		rec.calls++
		rec.payload = payload
		rec.mu.Unlock()
		io.WriteString(w, `{"code":200,"message":"success"}`)
	}))
	defer barkSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "unipush.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	sender, err := bark.New(bark.Config{ServerURL: barkSrv.URL, DeviceKey: "key-1", Group: "unipush"})
	if err != nil {
		t.Fatalf("bark.New: %v", err)
	}

	dsp := dispatch.New(st, channel.Set{"bark": sender}, nil, dispatch.Config{
		Workers:        2,
		PollInterval:   5 * time.Millisecond,
		Lease:          time.Minute,
		MaxAttempts:    5,
		BaseBackoff:    5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		RequestTimeout: 2 * time.Second,
		Dedupe:         dispatch.DedupeConfig{Enabled: true, Window: 5 * time.Minute},
		Aggregation:    dispatch.AggregationConfig{Enabled: true, Window: 120 * time.Millisecond, MaxItems: 10},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dsp.Run(dispatchCtx)
	}()
	defer func() {
		stopDispatch()
		<-dispatchDone
	}()

	server := New(Config{AuthTokens: []string{testToken}, MaxBodyBytes: 1 << 20}, st, dsp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// —— 去重：同一 dedupe_key 连发 10 次 ——
	var firstID string
	for i := 1; i <= 10; i++ {
		body := `{"title":"磁盘告警","body":"剩余 3%","level":"error","source":"nas-monitor","dedupe_key":"nas-disk-42"}`
		resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, body)
		if resp.StatusCode != http.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("第 %d 次创建消息状态码 = %d, body=%s", i, resp.StatusCode, raw)
		}
		var created createResponse
		decodeBody(t, resp, &created)

		if i == 1 {
			if created.Status != "queued" {
				t.Fatalf("首条消息 status = %q，期望 queued", created.Status)
			}
			if len(created.Deliveries) != 1 {
				t.Fatalf("首条消息应只有 1 条投递（唯一启用的 bark），实际 %+v", created.Deliveries)
			}
			firstID = created.ID
			continue
		}
		if created.Status != "deduped" {
			t.Fatalf("第 %d 次 status = %q，期望 deduped", i, created.Status)
		}
		if created.ID != firstID {
			t.Errorf("第 %d 次返回的 ID = %q，期望原消息 %q", i, created.ID, firstID)
		}
		if created.Merged != i-1 {
			t.Errorf("第 %d 次 merged = %d，期望 %d", i, created.Merged, i-1)
		}
		if len(created.Deliveries) != 0 {
			t.Errorf("去重命中时不应产生新的投递，实际 %+v", created.Deliveries)
		}
	}

	getResp := requestJSON(t, ts, http.MethodGet, "/v1/messages/"+firstID, testToken, "")
	var stored getResponse
	decodeBody(t, getResp, &stored)
	if stored.Message.MergedCount != 9 {
		t.Errorf("记录里的 merged_count = %d，期望 9", stored.Message.MergedCount)
	}
	if len(stored.Deliveries) != 1 {
		t.Errorf("总共应只有 1 条投递记录，实际 %d", len(stored.Deliveries))
	}

	// —— 聚合：三条 info 消息合成一条 digest ——
	for i := 1; i <= 3; i++ {
		body := fmt.Sprintf(`{"title":"备份完成 %d","body":"耗时 32s","level":"info","source":"backup"}`, i)
		resp := requestJSON(t, ts, http.MethodPost, "/v1/messages", testToken, body)
		var created createResponse
		decodeBody(t, resp, &created)
		if created.Status != "aggregated" {
			t.Fatalf("info 消息应返回 aggregated，实际 %q", created.Status)
		}
		if len(created.Deliveries) != 0 {
			t.Errorf("聚合中的消息不应立即投递，实际 %+v", created.Deliveries)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		calls, payload := rec.snapshot()
		if calls == 2 {
			title, _ := payload["title"].(string)
			if title != "3 条 backup 汇总" {
				t.Errorf("digest 标题 = %q，期望 %q", title, "3 条 backup 汇总")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 digest 投递超时，bark 调用次数 = %d（期望 2：1 次去重投递 + 1 次 digest）", calls)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
