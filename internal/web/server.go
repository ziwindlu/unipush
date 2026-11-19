// Package web 提供 unipush 的 Web 控制台：登录、消息查看、发送与 Token / 用户管理。
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/auth"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Queue 是 Web 控制台需要的投递能力。
type Queue interface {
	Channels() []string
	Groups() []string
	Enqueue(ctx context.Context, msg *model.Message) (*model.EnqueueResult, error)
}

// Config 是 Web 控制台配置。
type Config struct {
	// Version 显示在页脚，方便确认线上版本。
	Version string
	// SessionTTL 是登录会话有效期。
	SessionTTL time.Duration
	// MaxBodyBytes 是表单请求体上限。
	MaxBodyBytes int64
}

// Server 是 Web 控制台处理器。
type Server struct {
	cfg     Config
	store   *store.Store
	queue   Queue
	log     *slog.Logger
	mux     *http.ServeMux
	pages   map[string]*template.Template
	flash   *flashStore
	limiter *loginLimiter
}

// New 创建 Web 控制台；模板解析失败会直接返回错误。
func New(cfg Config, st *store.Store, queue Queue, logger *slog.Logger) (*Server, error) {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if logger == nil {
		logger = slog.Default()
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		store:   st,
		queue:   queue,
		log:     logger,
		mux:     http.NewServeMux(),
		pages:   pages,
		flash:   newFlashStore(),
		limiter: newLoginLimiter(5, 5*time.Minute),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleRoot)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	s.mux.HandleFunc("GET /app", s.withUser(s.handleDashboard))
	s.mux.HandleFunc("GET /app/messages", s.withUser(s.handleMessages))
	s.mux.HandleFunc("GET /app/messages/{id}", s.withUser(s.handleMessage))
	s.mux.HandleFunc("GET /app/send", s.withUser(s.handleSendPage))
	s.mux.HandleFunc("POST /app/send", s.withUser(s.handleSend))
	s.mux.HandleFunc("GET /app/tokens", s.withUser(s.handleTokens))
	s.mux.HandleFunc("POST /app/tokens", s.withUser(s.handleCreateToken))
	s.mux.HandleFunc("POST /app/tokens/{id}/revoke", s.withUser(s.handleRevokeToken))
	s.mux.HandleFunc("GET /app/users", s.withAdmin(s.handleUsers))
	s.mux.HandleFunc("POST /app/users", s.withAdmin(s.handleCreateUser))
	s.mux.HandleFunc("POST /app/users/{id}/password", s.withAdmin(s.handleResetPassword))
	s.mux.HandleFunc("POST /app/users/{id}/status", s.withAdmin(s.handleUserStatus))
	s.mux.HandleFunc("GET /app/account", s.withUser(s.handleAccount))
	s.mux.HandleFunc("POST /app/account/password", s.withUser(s.handleChangePassword))

	s.mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	s.mux.HandleFunc("/", s.handleNotFound)
}

// Handler 返回带安全响应头与访问日志的处理器。
func (s *Server) Handler() http.Handler {
	return s.withSecurityHeaders(s.withAccessLog(s.mux))
}

type pageData struct {
	Title   string
	Active  string
	User    *store.User
	CSRF    string
	Version string
	Flash   *flashMessage
	Data    any
}

type userHandler func(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session)

func (s *Server) withUser(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, sess, err := s.currentSession(r)
		if err != nil {
			http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, r, user, sess)
	}
}

func (s *Server) withAdmin(next userHandler) http.HandlerFunc {
	return s.withUser(func(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
		if !user.IsAdmin() {
			s.renderError(w, r, user, sess, http.StatusForbidden, "需要管理员权限", "")
			return
		}
		next(w, r, user, sess)
	})
}

// currentSession 读取会话 Cookie 并返回用户；未登录时返回 ErrNotFound。
func (s *Server) currentSession(r *http.Request) (*store.User, *store.Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, nil, store.ErrNotFound
	}
	now := time.Now().UTC()
	user, sess, err := s.store.GetSession(r.Context(), hashToken(cookie.Value), now)
	if err != nil {
		return nil, nil, err
	}
	return user, sess, nil
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.currentSession(r); err == nil {
		http.Redirect(w, r, "/app", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	user, sess, _ := s.currentSession(r)
	s.renderError(w, r, user, sess, http.StatusNotFound, "页面不存在", "检查地址是否输入正确。")
}

// checkCSRF 校验表单里的 csrf_token 与会话绑定值一致。所有写操作都必须先调用它。
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, user, sess, http.StatusBadRequest, "表单格式错误", err.Error())
		return false
	}
	if !constantTimeEqual(r.PostFormValue("csrf_token"), sess.CSRFToken) {
		s.renderError(w, r, user, sess, http.StatusForbidden, "表单已过期", "请刷新页面后重试。")
		return false
	}
	return true
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session, page, title, active string, data any) {
	tpl, ok := s.pages[page]
	if !ok {
		s.log.Error("模板不存在", "page", page)
		http.Error(w, "模板不存在", http.StatusInternalServerError)
		return
	}
	pd := &pageData{
		Title:   title,
		Active:  active,
		User:    user,
		Version: s.cfg.Version,
		Data:    data,
	}
	if sess != nil {
		pd.CSRF = sess.CSRFToken
		pd.Flash = s.flash.Pop(sess.TokenHash)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := tpl.ExecuteTemplate(w, "base", pd); err != nil {
		s.log.Error("渲染页面失败", "page", page, "error", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session, status int, title, detail string) {
	tpl, ok := s.pages["error"]
	if !ok {
		http.Error(w, title, status)
		return
	}
	pd := &pageData{Title: title, User: user, Version: s.cfg.Version, Data: map[string]string{
		"Title":  title,
		"Detail": detail,
	}}
	if sess != nil {
		pd.CSRF = sess.CSRFToken
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "base", pd); err != nil {
		s.log.Error("渲染错误页失败", "error", err)
	}
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
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
		s.log.Info("web 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
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

// clientIP 只信任 RemoteAddr；不解析 X-Forwarded-For，避免绕过登录限流。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// BootstrapAdmin 在数据库里没有任何用户时创建初始管理员。
// 返回生成的密码（仅当密码是随机生成时），供启动日志展示一次。
func BootstrapAdmin(ctx context.Context, st *store.Store, username, password string, logger *slog.Logger) (string, error) {
	count, err := st.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	if strings.TrimSpace(username) == "" {
		username = "admin"
	}
	generated := ""
	if password == "" {
		password, err = auth.GeneratePassword(16)
		if err != nil {
			return "", err
		}
		generated = password
	}
	if len(password) < 8 {
		return "", fmt.Errorf("初始管理员密码至少 8 个字符")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	user := &store.User{
		ID:          model.NewID("usr_"),
		Username:    strings.ToLower(strings.TrimSpace(username)),
		DisplayName: "管理员",
		Role:        store.RoleAdmin,
	}
	if err := st.CreateUser(ctx, user, hash); err != nil {
		return "", err
	}
	if logger != nil {
		if generated != "" {
			logger.Warn("首次启动已创建 Web 控制台管理员，随机密码只打印这一次，请登录后尽快修改",
				"username", user.Username, "password", generated)
		} else {
			logger.Info("首次启动已创建 Web 控制台管理员", "username", user.Username)
		}
	}
	return generated, nil
}
