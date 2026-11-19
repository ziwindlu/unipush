// Package api 实现 HTTP 接口：消息入口、投递状态查询与健康检查。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/routing"
	"github.com/ziwindlu/unipush/internal/store"
)

// Store 是 API 需要的持久化能力。
type Store interface {
	GetMessage(ctx context.Context, id string) (*model.Message, error)
	ListMessages(ctx context.Context, filter store.MessageFilter) ([]*store.MessageListItem, bool, error)
	ListDeliveries(ctx context.Context, messageID string) ([]*model.Delivery, error)
	ListAggregatedMessages(ctx context.Context, digestID string) ([]string, error)
	DeliveryStats(ctx context.Context) (store.DeliveryQueueStats, error)
	Ping(ctx context.Context) error
}

// Queue 是 API 需要的投递能力。
type Queue interface {
	Channels() []string
	Groups() []string
	Enqueue(ctx context.Context, msg *model.Message) (*model.EnqueueResult, error)
}

// Config 是 API 配置。
type Config struct {
	// AuthTokens 是允许的 Bearer Token 列表。
	AuthTokens []string
	// MaxBodyBytes 是请求体大小上限。
	MaxBodyBytes int64
	// Version 会出现在 /healthz 里，方便确认线上跑的是哪个版本。
	Version string
}

// Server 是 HTTP 处理器。
type Server struct {
	cfg       Config
	store     Store
	queue     Queue
	log       *slog.Logger
	mux       *http.ServeMux
	startedAt time.Time
}

// New 创建 API Server。
func New(cfg Config, st Store, queue Queue, logger *slog.Logger) *Server {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{cfg: cfg, store: st, queue: queue, log: logger, mux: http.NewServeMux(), startedAt: time.Now()}

	s.mux.Handle("POST /v1/messages", s.requireAuth(http.HandlerFunc(s.handleCreateMessage)))
	s.mux.Handle("GET /v1/messages", s.requireAuth(http.HandlerFunc(s.handleListMessages)))
	s.mux.Handle("GET /v1/messages/{id}", s.requireAuth(http.HandlerFunc(s.handleGetMessage)))
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("/", s.handleNotFound)
	return s
}

// Handler 返回带访问日志的 HTTP 处理器。
func (s *Server) Handler() http.Handler { return s.withAccessLog(s.mux) }

// createRequest 是 POST /v1/messages 的请求体。
type createRequest struct {
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Level     string         `json:"level"`
	Source    string         `json:"source"`
	Targets   []string       `json:"targets"`
	DedupeKey string         `json:"dedupe_key"`
	Link      string         `json:"link"`
	Extra     map[string]any `json:"extra"`
}

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	var req createRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("请求体超过 %d 字节上限", s.cfg.MaxBodyBytes), "")
			return
		}
		if errors.Is(err, io.EOF) {
			s.writeError(w, http.StatusBadRequest, "invalid_request", "请求体不能为空", "")
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid_json", "请求体不是合法的 JSON: "+err.Error(), "")
		return
	}
	if err := ensureEOF(dec); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json", "请求体只能包含一个 JSON 对象", "")
		return
	}

	msg, verr := s.buildMessage(&req)
	if verr != nil {
		s.writeError(w, verr.status, verr.code, verr.message, verr.field)
		return
	}

	result, err := s.queue.Enqueue(r.Context(), msg)
	if err != nil {
		s.writeEnqueueError(w, msg.ID, err)
		return
	}

	createdAt := msg.CreatedAt
	merged := result.Merged
	if result.Status == model.EnqueueDeduped {
		// 命中去重时返回原消息的创建时间与累计合并次数，调用方据此判断「这是第几次重复」。
		if original, err := s.store.GetMessage(r.Context(), result.MessageID); err == nil {
			createdAt = original.CreatedAt
			merged = original.MergedCount
		}
	}

	resp := createResponse{
		ID:         result.MessageID,
		Status:     string(result.Status),
		Merged:     merged,
		CreatedAt:  createdAt,
		Route:      result.Route,
		Deliveries: make([]deliverySummary, 0, len(result.Deliveries)),
	}
	for _, d := range result.Deliveries {
		resp.Deliveries = append(resp.Deliveries, deliverySummary{
			ID:      d.ID,
			Channel: d.Channel,
			Status:  string(d.Status),
			Groups:  d.Groups,
		})
	}
	w.Header().Set("Location", "/v1/messages/"+result.MessageID)
	s.writeJSON(w, http.StatusAccepted, resp)
}

type validationError struct {
	status  int
	code    string
	message string
	field   string
}

func (s *Server) buildMessage(req *createRequest) (*model.Message, *validationError) {
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	if title == "" && body == "" {
		return nil, &validationError{http.StatusBadRequest, "invalid_request", "title 与 body 不能同时为空", "title"}
	}
	if utf8.RuneCountInString(title) > 200 {
		return nil, &validationError{http.StatusBadRequest, "invalid_request", "title 不能超过 200 个字符", "title"}
	}
	if utf8.RuneCountInString(body) > 8000 {
		return nil, &validationError{http.StatusBadRequest, "invalid_request", "body 不能超过 8000 个字符", "body"}
	}

	level := strings.ToLower(strings.TrimSpace(req.Level))
	if level == "" {
		level = model.LevelInfo
	}
	if !model.ValidLevel(level) {
		return nil, &validationError{http.StatusBadRequest, "invalid_request", "level 必须是 info / warn / error 之一", "level"}
	}

	source := strings.TrimSpace(req.Source)
	if utf8.RuneCountInString(source) > 64 {
		return nil, &validationError{http.StatusBadRequest, "invalid_request", "source 不能超过 64 个字符", "source"}
	}
	dedupeKey := strings.TrimSpace(req.DedupeKey)
	if utf8.RuneCountInString(dedupeKey) > 256 {
		return nil, &validationError{http.StatusBadRequest, "invalid_request", "dedupe_key 不能超过 256 个字符", "dedupe_key"}
	}

	link := strings.TrimSpace(req.Link)
	if link != "" {
		u, err := url.Parse(link)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, &validationError{http.StatusBadRequest, "invalid_request", "link 必须是 http(s) 绝对地址", "link"}
		}
	}

	targets := model.NormalizeTargets(req.Targets)
	available := s.queue.Channels()
	groups := s.queue.Groups()
	if len(targets) > 0 {
		known := make(map[string]struct{}, len(available)+len(groups))
		for _, name := range groups {
			known[name] = struct{}{}
		}
		for _, name := range available {
			known[name] = struct{}{}
		}
		for _, name := range targets {
			if _, ok := known[name]; !ok {
				return nil, &validationError{
					http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("未知投递目标 %q（可用渠道组: %s；可用渠道: %s）",
						name, joinNames(groups), joinNames(available)),
					"targets",
				}
			}
		}
	} else if len(available) == 0 {
		return nil, &validationError{http.StatusServiceUnavailable, "no_channels", "服务未配置任何可用渠道", "targets"}
	}

	return &model.Message{
		ID:        model.NewID("msg_"),
		Title:     title,
		Body:      body,
		Level:     level,
		Source:    source,
		Targets:   targets,
		DedupeKey: dedupeKey,
		Link:      link,
		Extra:     req.Extra,
		CreatedAt: time.Now().UTC(),
	}, nil
}

type createResponse struct {
	ID string `json:"id"`
	// Status 是入队结果：queued（已排投递）/ deduped（命中去重）/ aggregated（进入聚合窗口）。
	Status string `json:"status"`
	// Merged 是同一 dedupe_key 被合并掉的次数，仅 deduped 时非零。
	Merged     int               `json:"merged,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Route      *model.Route      `json:"route,omitempty"`
	Deliveries []deliverySummary `json:"deliveries"`
}

type deliverySummary struct {
	ID      string   `json:"id"`
	Channel string   `json:"channel"`
	Status  string   `json:"status"`
	Groups  []string `json:"groups,omitempty"`
}

// writeEnqueueError 把路由与入队错误翻译成调用方能行动的 HTTP 响应：
// 目标写错 → 400（field=targets）；目标没有可用渠道 → 503；其余 → 500。
func (s *Server) writeEnqueueError(w http.ResponseWriter, messageID string, err error) {
	var unknown *routing.UnknownTargetError
	if errors.As(err, &unknown) {
		s.writeError(w, http.StatusBadRequest, "invalid_request", unknown.Error(), "targets")
		return
	}
	var noRoute *routing.NoRouteError
	if errors.As(err, &noRoute) {
		s.writeError(w, http.StatusServiceUnavailable, "no_route", noRoute.Error(), "targets")
		return
	}
	s.log.Error("消息入队失败", "error", err, "message_id", messageID)
	s.writeError(w, http.StatusInternalServerError, "enqueue_failed", "消息入队失败: "+err.Error(), "")
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, ", ")
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msg, err := s.store.GetMessage(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "消息不存在: "+id, "")
			return
		}
		s.log.Error("读取消息失败", "error", err, "message_id", id)
		s.writeError(w, http.StatusInternalServerError, "internal_error", "读取消息失败", "")
		return
	}
	deliveries, err := s.store.ListDeliveries(r.Context(), id)
	if err != nil {
		s.log.Error("读取投递记录失败", "error", err, "message_id", id)
		s.writeError(w, http.StatusInternalServerError, "internal_error", "读取投递记录失败", "")
		return
	}
	if deliveries == nil {
		deliveries = []*model.Delivery{}
	}
	aggregated, err := s.store.ListAggregatedMessages(r.Context(), id)
	if err != nil {
		s.log.Error("读取 digest 组成失败", "error", err, "message_id", id)
		s.writeError(w, http.StatusInternalServerError, "internal_error", "读取 digest 组成失败", "")
		return
	}
	s.writeJSON(w, http.StatusOK, getResponse{
		Message:       msg,
		Status:        aggregateStatus(msg, deliveries),
		Deliveries:    deliveries,
		AggregatedIDs: aggregated,
	})
}

type getResponse struct {
	Message    *model.Message    `json:"message"`
	Status     string            `json:"status"`
	Deliveries []*model.Delivery `json:"deliveries"`
	// AggregatedIDs 是这条 digest 汇总的原始消息 ID，仅 digest 非空。
	AggregatedIDs []string `json:"aggregated_ids,omitempty"`
}

func aggregateStatus(msg *model.Message, deliveries []*model.Delivery) string {
	var counts model.DeliveryCounts
	for _, d := range deliveries {
		counts.Add(d.Status)
	}
	// 已汇入 digest 的原始 info 消息自己不会再有投递记录。
	return counts.Status(msg != nil && msg.AggregatedInto != "")
}

// healthResponse 是 /healthz 的响应体。数据库不可用时返回 503，
// 队列积压不算不健康（消息还在重试），只是把数字暴露出来便于排查。
type healthResponse struct {
	Status        string                    `json:"status"`
	Version       string                    `json:"version,omitempty"`
	UptimeSeconds int64                     `json:"uptime_seconds"`
	Checks        map[string]string         `json:"checks"`
	Queue         *store.DeliveryQueueStats `json:"queue,omitempty"`
	Channels      []string                  `json:"channels,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := healthResponse{
		Status:        "ok",
		Version:       s.cfg.Version,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Checks:        map[string]string{"database": "ok"},
		Channels:      s.queue.Channels(),
	}
	status := http.StatusOK
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("健康检查失败", "error", err)
		resp.Status = "unavailable"
		resp.Checks["database"] = "unavailable"
		status = http.StatusServiceUnavailable
	} else if stats, err := s.store.DeliveryStats(ctx); err != nil {
		s.log.Error("健康检查读取队列统计失败", "error", err)
		resp.Status = "degraded"
		resp.Checks["queue"] = "unavailable"
	} else {
		resp.Queue = &stats
	}
	s.writeJSON(w, status, resp)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusNotFound, "not_found", "接口不存在: "+r.Method+" "+r.URL.Path, "")
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="unipush"`)
			s.writeError(w, http.StatusUnauthorized, "unauthorized", "无效或缺失的 Bearer Token", "authorization")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return false
	}
	for _, want := range s.cfg.AuthTokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("写响应失败", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message, field string) {
	s.writeJSON(w, status, errorBody{Error: apiError{Code: code, Message: message, Field: field}})
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("多余的 JSON 内容")
		}
		return err
	}
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.Info("http 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}
