package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ziwindlu/unipush/internal/ingest"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/routing"
	"github.com/ziwindlu/unipush/internal/store"
)

type dashboardData struct {
	Stats    store.DeliveryQueueStats
	Channels []string
	Groups   []string
	Recent   []*store.MessageListItem
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	stats, err := s.store.DeliveryStats(r.Context())
	if err != nil {
		s.log.Error("读取队列统计失败", "error", err)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取队列统计失败", "")
		return
	}
	recent, _, err := s.store.ListMessages(r.Context(), store.MessageFilter{Limit: 8})
	if err != nil {
		s.log.Error("读取最近消息失败", "error", err)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取最近消息失败", "")
		return
	}
	s.render(w, r, user, sess, "dashboard", "概览", "dashboard", dashboardData{
		Stats:    stats,
		Channels: s.queue.Channels(),
		Groups:   s.queue.Groups(),
		Recent:   recent,
	})
}

type messagesData struct {
	Items      []*store.MessageListItem
	HasMore    bool
	NextURL    string
	Source     string
	Level      string
	Status     string
	TotalShown int
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	q := r.URL.Query()
	filter := store.MessageFilter{
		Source: strings.TrimSpace(q.Get("source")),
		Level:  strings.ToLower(strings.TrimSpace(q.Get("level"))),
		Status: strings.ToLower(strings.TrimSpace(q.Get("status"))),
		Limit:  20,
	}
	if filter.Level != "" && !model.ValidLevel(filter.Level) {
		s.renderError(w, r, user, sess, http.StatusBadRequest, "过滤条件不正确", "level 必须是 info / warn / error。")
		return
	}
	if filter.Status != "" && !model.ValidStatus(filter.Status) {
		s.renderError(w, r, user, sess, http.StatusBadRequest, "过滤条件不正确", "status 取值不合法。")
		return
	}
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		cursor, err := store.DecodeCursor(raw)
		if err != nil {
			s.renderError(w, r, user, sess, http.StatusBadRequest, "分页游标无效", "请从消息列表重新进入。")
			return
		}
		filter.Before = cursor
	}
	items, hasMore, err := s.store.ListMessages(r.Context(), filter)
	if err != nil {
		s.log.Error("查询消息列表失败", "error", err)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "查询消息列表失败", "")
		return
	}

	data := messagesData{
		Items:      items,
		HasMore:    hasMore,
		Source:     filter.Source,
		Level:      filter.Level,
		Status:     filter.Status,
		TotalShown: len(items),
	}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1].Message
		values := url.Values{}
		if filter.Source != "" {
			values.Set("source", filter.Source)
		}
		if filter.Level != "" {
			values.Set("level", filter.Level)
		}
		if filter.Status != "" {
			values.Set("status", filter.Status)
		}
		values.Set("cursor", store.EncodeCursor(store.MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID}))
		data.NextURL = "/app/messages?" + values.Encode()
	}
	s.render(w, r, user, sess, "messages", "消息", "messages", data)
}

type messageData struct {
	Message       *model.Message
	Status        string
	Deliveries    []*model.Delivery
	AggregatedIDs []string
	RawJSON       string
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	id := r.PathValue("id")
	msg, err := s.store.GetMessage(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderError(w, r, user, sess, http.StatusNotFound, "消息不存在", "这条消息可能已被清理。")
			return
		}
		s.log.Error("读取消息失败", "error", err, "message_id", id)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取消息失败", "")
		return
	}
	deliveries, err := s.store.ListDeliveries(r.Context(), id)
	if err != nil {
		s.log.Error("读取投递记录失败", "error", err, "message_id", id)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取投递记录失败", "")
		return
	}
	aggregated, err := s.store.ListAggregatedMessages(r.Context(), id)
	if err != nil {
		s.log.Error("读取聚合消息失败", "error", err, "message_id", id)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取聚合消息失败", "")
		return
	}
	var counts model.DeliveryCounts
	for _, d := range deliveries {
		counts.Add(d.Status)
	}
	s.render(w, r, user, sess, "message", "消息详情", "messages", messageData{
		Message:       msg,
		Status:        counts.Status(msg.AggregatedInto != ""),
		Deliveries:    deliveries,
		AggregatedIDs: aggregated,
		RawJSON:       jsonPrettyFor(msg),
	})
}

type sendData struct {
	Channels []string
	Groups   []string
	Values   sendValues
	Error    string
}

type sendValues struct {
	Title     string
	Body      string
	Level     string
	Source    string
	Targets   []string
	DedupeKey string
	Link      string
}

func (s *Server) sendPageData(values sendValues, message string) sendData {
	if values.Level == "" {
		values.Level = model.LevelInfo
	}
	if values.Source == "" {
		values.Source = "web"
	}
	return sendData{
		Channels: s.queue.Channels(),
		Groups:   s.queue.Groups(),
		Values:   values,
		Error:    message,
	}
}

func (s *Server) handleSendPage(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	s.render(w, r, user, sess, "send", "发送消息", "send", s.sendPageData(sendValues{}, ""))
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	values := sendValues{
		Title:     r.PostFormValue("title"),
		Body:      r.PostFormValue("body"),
		Level:     r.PostFormValue("level"),
		Source:    r.PostFormValue("source"),
		Targets:   r.PostForm["targets"],
		DedupeKey: r.PostFormValue("dedupe_key"),
		Link:      r.PostFormValue("link"),
	}
	req := &ingest.Request{
		Title:     values.Title,
		Body:      values.Body,
		Level:     values.Level,
		Source:    values.Source,
		Targets:   values.Targets,
		DedupeKey: values.DedupeKey,
		Link:      values.Link,
	}
	msg, verr := ingest.Build(req, s.queue.Channels(), s.queue.Groups())
	if verr != nil {
		s.renderSendError(w, r, user, sess, verr.Status, values, verr.Message)
		return
	}
	result, err := s.queue.Enqueue(r.Context(), msg)
	if err != nil {
		var unknown *routing.UnknownTargetError
		var noRoute *routing.NoRouteError
		switch {
		case errors.As(err, &unknown):
			s.renderSendError(w, r, user, sess, http.StatusBadRequest, values, unknown.Error())
		case errors.As(err, &noRoute):
			s.renderSendError(w, r, user, sess, http.StatusServiceUnavailable, values, noRoute.Error())
		default:
			s.log.Error("Web 发送消息入队失败", "error", err, "message_id", msg.ID)
			s.renderSendError(w, r, user, sess, http.StatusInternalServerError, values, "消息入队失败，请查看服务日志。")
		}
		return
	}
	text := "消息已受理，正在投递。"
	switch result.Status {
	case model.EnqueueDeduped:
		text = fmt.Sprintf("命中去重窗口，已合并到原消息（累计合并 %d 次）。", result.Merged)
	case model.EnqueueAggregated:
		text = "消息已进入聚合窗口，稍后随 digest 一起发送。"
	}
	s.flash.Set(sess.TokenHash, flashMessage{Kind: "success", Text: text})
	s.redirect(w, r, "/app/messages/"+result.MessageID)
}

func (s *Server) renderSendError(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session, status int, values sendValues, message string) {
	tpl, ok := s.pages["send"]
	if !ok {
		http.Error(w, message, status)
		return
	}
	pd := &pageData{
		Title:   "发送消息",
		Active:  "send",
		User:    user,
		CSRF:    sess.CSRFToken,
		Version: s.cfg.Version,
		Flash:   s.flash.Pop(sess.TokenHash),
		Data:    s.sendPageData(values, message),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "base", pd); err != nil {
		s.log.Error("渲染发送页失败", "error", err)
	}
}

func jsonPrettyFor(v any) string {
	return jsonPretty(v)
}
