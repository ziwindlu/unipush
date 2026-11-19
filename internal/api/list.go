package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

// 分页大小上下限：默认 20，最多 200，避免一次拉全库。
const (
	defaultListLimit = 20
	maxListLimit     = 200
)

// listResponse 是 GET /v1/messages 的响应体。
type listResponse struct {
	Messages []listItem `json:"messages"`
	// NextCursor 非空时表示还有下一页，原样回传给 cursor 参数。
	NextCursor string `json:"next_cursor,omitempty"`
}

// listItem 是列表里的一条消息：消息字段平铺，另加聚合状态与各渠道投递摘要。
type listItem struct {
	*model.Message
	Status     string         `json:"status"`
	Deliveries []listDelivery `json:"deliveries"`
}

// listDelivery 是列表页需要的投递摘要，比详情接口少一些字段。
type listDelivery struct {
	ID            string     `json:"id"`
	Channel       string     `json:"channel"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}

// handleListMessages 提供只读消息列表：按创建时间倒序，支持来源/级别/状态过滤与游标分页。
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := defaultListLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLimit {
			s.writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("limit 必须是 1-%d 之间的整数", maxListLimit), "limit")
			return
		}
		limit = n
	}

	source := strings.TrimSpace(q.Get("source"))
	if utf8.RuneCountInString(source) > 64 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "source 不能超过 64 个字符", "source")
		return
	}
	level := strings.ToLower(strings.TrimSpace(q.Get("level")))
	if level != "" && !model.ValidLevel(level) {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "level 必须是 info / warn / error 之一", "level")
		return
	}
	status := strings.ToLower(strings.TrimSpace(q.Get("status")))
	if status != "" && !model.ValidStatus(status) {
		s.writeError(w, http.StatusBadRequest, "invalid_request",
			"status 必须是 queued / aggregated / in_progress / succeeded / partial / failed 之一", "status")
		return
	}

	var since *time.Time
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_request", "since 必须是 RFC3339 时间，例如 2026-01-01T00:00:00Z", "since")
			return
		}
		since = &parsed
	}

	var before *store.MessageCursor
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_request", "cursor 无效，请使用上一页返回的 next_cursor", "cursor")
			return
		}
		before = cursor
	}

	items, hasMore, err := s.store.ListMessages(r.Context(), store.MessageFilter{
		Source: source,
		Level:  level,
		Status: status,
		Since:  since,
		Before: before,
		Limit:  limit,
	})
	if err != nil {
		s.log.Error("查询消息列表失败", "error", err)
		s.writeError(w, http.StatusInternalServerError, "internal_error", "查询消息列表失败", "")
		return
	}

	resp := listResponse{Messages: make([]listItem, 0, len(items))}
	for _, item := range items {
		entry := listItem{Message: item.Message, Status: item.Status, Deliveries: make([]listDelivery, 0, len(item.Deliveries))}
		for _, d := range item.Deliveries {
			entry.Deliveries = append(entry.Deliveries, listDelivery{
				ID:            d.ID,
				Channel:       d.Channel,
				Status:        string(d.Status),
				Attempts:      d.Attempts,
				LastError:     d.LastError,
				NextAttemptAt: d.NextAttemptAt,
			})
		}
		resp.Messages = append(resp.Messages, entry)
	}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1].Message
		resp.NextCursor = encodeCursor(store.MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// encodeCursor 把分页位置编码成不透明字符串，避免调用方依赖内部列。
func encodeCursor(c store.MessageCursor) string {
	raw := fmt.Sprintf("%d:%s", c.CreatedAt.UTC().UnixMilli(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(encoded string) (*store.MessageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	millis, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" {
		return nil, fmt.Errorf("cursor 格式不正确")
	}
	var n int64
	if _, err := fmt.Sscanf(millis, "%d", &n); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, fmt.Errorf("cursor 时间无效")
	}
	return &store.MessageCursor{CreatedAt: time.UnixMilli(n).UTC(), ID: id}, nil
}
