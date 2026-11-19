package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

func listItemFor(id, source string, createdAt time.Time, status string) *store.MessageListItem {
	msg := &model.Message{
		ID:        id,
		Title:     "标题 " + id,
		Body:      "正文",
		Level:     model.LevelWarn,
		Source:    source,
		CreatedAt: createdAt,
	}
	return &store.MessageListItem{
		Message: msg,
		Status:  status,
		Deliveries: []*model.Delivery{{
			ID:        "dlv_" + id,
			MessageID: id,
			Channel:   "bark",
			Status:    model.DeliverySucceeded,
			Attempts:  1,
		}},
	}
}

func TestListMessagesRequiresAuth(t *testing.T) {
	ts := newTestAPI(t, &fakeStore{}, &fakeQueue{channels: []string{"bark"}})
	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 状态码 = %d, 期望 401", resp.StatusCode)
	}
}

// TestListMessagesPassesFilters 覆盖 M4：按来源/级别/状态过滤与分页参数都透传到存储层。
func TestListMessagesPassesFilters(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeStore{listItems: []*store.MessageListItem{
		listItemFor("msg_b", "ci", now, "succeeded"),
		listItemFor("msg_a", "ci", now.Add(-time.Minute), "failed"),
	}}
	ts := newTestAPI(t, st, &fakeQueue{channels: []string{"bark"}})

	sinceTime := now.Add(-time.Hour).Truncate(time.Second)
	since := sinceTime.Format(time.RFC3339)
	cursor := store.EncodeCursor(store.MessageCursor{CreatedAt: now.Add(-2 * time.Minute), ID: "msg_c"})
	path := "/v1/messages?" + url.Values{
		"limit":  {"2"},
		"source": {"ci"},
		"level":  {"warn"},
		"status": {"failed"},
		"since":  {since},
		"cursor": {cursor},
	}.Encode()

	resp := requestJSON(t, ts, http.MethodGet, path, testToken, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if st.lastFilter.Source != "ci" || st.lastFilter.Level != "warn" || st.lastFilter.Status != "failed" || st.lastFilter.Limit != 2 {
		t.Errorf("过滤条件未透传: %+v", st.lastFilter)
	}
	if st.lastFilter.Since == nil || !st.lastFilter.Since.Equal(sinceTime) {
		t.Errorf("since 未透传: %v", st.lastFilter.Since)
	}
	if st.lastFilter.Before == nil || st.lastFilter.Before.ID != "msg_c" {
		t.Errorf("cursor 未透传: %+v", st.lastFilter.Before)
	}

	var body struct {
		Messages []struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			Deliveries []struct {
				Channel  string `json:"channel"`
				Status   string `json:"status"`
				Attempts int    `json:"attempts"`
			} `json:"deliveries"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(body.Messages) != 2 || body.Messages[0].ID != "msg_b" || body.Messages[0].Status != "succeeded" {
		t.Fatalf("列表响应不对: %+v", body.Messages)
	}
	if len(body.Messages[0].Deliveries) != 1 || body.Messages[0].Deliveries[0].Channel != "bark" {
		t.Errorf("投递摘要不对: %+v", body.Messages[0].Deliveries)
	}
}

func TestListMessagesNextCursor(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeStore{
		listItems:   []*store.MessageListItem{listItemFor("msg_a", "ci", now, "queued")},
		listHasMore: true,
	}
	ts := newTestAPI(t, st, &fakeQueue{channels: []string{"bark"}})
	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages", testToken, "")
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	next, _ := body["next_cursor"].(string)
	if next == "" {
		t.Fatal("还有下一页时应返回 next_cursor")
	}
	cursor, err := store.DecodeCursor(next)
	if err != nil {
		t.Fatalf("next_cursor 无法解码: %v", err)
	}
	if cursor.ID != "msg_a" {
		t.Errorf("next_cursor 指向 %q, 期望 msg_a", cursor.ID)
	}
}

func TestListMessagesRejectsInvalidParams(t *testing.T) {
	ts := newTestAPI(t, &fakeStore{}, &fakeQueue{})
	cases := []struct{ query, field string }{
		{"limit=0", "limit"},
		{"limit=201", "limit"},
		{"limit=abc", "limit"},
		{"level=fatal", "level"},
		{"status=lost", "status"},
		{"since=yesterday", "since"},
		{"cursor=not-base64", "cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp := requestJSON(t, ts, http.MethodGet, "/v1/messages?"+tc.query, testToken, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
			}
			var body struct {
				Error struct {
					Code  string `json:"code"`
					Field string `json:"field"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("解析错误响应: %v", err)
			}
			if body.Error.Field != tc.field {
				t.Errorf("error.field = %q, 期望 %q", body.Error.Field, tc.field)
			}
		})
	}
}

func TestListMessagesEmpty(t *testing.T) {
	ts := newTestAPI(t, &fakeStore{}, &fakeQueue{})
	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages", testToken, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var body struct {
		Messages   []json.RawMessage `json:"messages"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body.Messages == nil || len(body.Messages) != 0 {
		t.Errorf("空列表应返回 []，实际 %v", body.Messages)
	}
	if body.NextCursor != "" {
		t.Errorf("空列表不应有 next_cursor: %q", body.NextCursor)
	}
}

func TestListMessagesStoreError(t *testing.T) {
	ts := newTestAPI(t, &fakeStore{listErr: fmt.Errorf("数据库坏了")}, &fakeQueue{})
	resp := requestJSON(t, ts, http.MethodGet, "/v1/messages", testToken, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", resp.StatusCode)
	}
}
