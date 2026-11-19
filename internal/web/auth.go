package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ziwindlu/unipush/internal/auth"
	"github.com/ziwindlu/unipush/internal/store"
)

const sessionCookieName = "unipush_session"

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.currentSession(r); err == nil {
		s.redirect(w, r, "/app")
		return
	}
	s.render(w, r, nil, nil, "login", "登录", "", map[string]string{
		"Next": safeNext(r.URL.Query().Get("next")),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.renderError(w, r, nil, nil, http.StatusForbidden, "请求来源不合法", "请从本站登录页提交表单。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.renderLoginError(w, r, http.StatusBadRequest, "表单格式错误")
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	key := clientIP(r) + "|" + username
	if !s.limiter.Allow(key) {
		s.renderLoginError(w, r, http.StatusTooManyRequests, "尝试次数过多，请 5 分钟后再试")
		return
	}
	if username == "" || password == "" {
		s.limiter.Fail(key)
		s.renderLoginError(w, r, http.StatusBadRequest, "请输入用户名和密码")
		return
	}

	user, hash, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("读取登录用户失败", "error", err, "username", username)
		}
		s.limiter.Fail(key)
		s.renderLoginError(w, r, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if user.Disabled || !auth.VerifyPassword(hash, password) {
		s.limiter.Fail(key)
		s.renderLoginError(w, r, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	now := time.Now().UTC()
	s.limiter.Reset(key)
	if _, err := s.store.CleanupExpiredSessions(r.Context(), now); err != nil {
		s.log.Warn("清理过期会话失败", "error", err)
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(cookie.Value)); err != nil {
			s.log.Warn("清理旧会话失败", "error", err)
		}
	}
	s.clearSessionCookie(w, r)
	if err := s.startSession(r.Context(), w, r, user, now); err != nil {
		s.log.Error("创建登录会话失败", "error", err, "user", user.Username)
		s.renderLoginError(w, r, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}
	if err := s.store.TouchUserLogin(r.Context(), user.ID, now); err != nil {
		s.log.Warn("记录登录时间失败", "error", err, "user", user.Username)
	}
	s.log.Info("web 登录成功", "username", user.Username, "remote", r.RemoteAddr)
	s.redirect(w, r, next)
}

func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, status int, message string) {
	tpl, ok := s.pages["login"]
	if !ok {
		http.Error(w, message, status)
		return
	}
	pd := &pageData{
		Title:   "登录",
		Version: s.cfg.Version,
		Data: map[string]string{
			"Next":  safeNext(r.FormValue("next")),
			"Error": message,
		},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "base", pd); err != nil {
		s.log.Error("渲染登录页失败", "error", err)
	}
}

// startSession 生成新的会话 token 与 CSRF token，并写入 Cookie。
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, user *store.User, now time.Time) error {
	token, tokenHash, err := auth.NewToken("sess_")
	if err != nil {
		return err
	}
	csrfToken, _, err := auth.NewToken("csrf_")
	if err != nil {
		return err
	}
	expires := now.Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(ctx, tokenHash, user.ID, csrfToken, now, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	})
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	user, sess, err := s.currentSession(r)
	if err != nil {
		s.clearSessionCookie(w, r)
		s.redirect(w, r, "/login")
		return
	}
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	if err := s.store.DeleteSession(r.Context(), sess.TokenHash); err != nil {
		s.log.Warn("删除会话失败", "error", err)
	}
	s.clearSessionCookie(w, r)
	s.redirect(w, r, "/login")
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	s.render(w, r, user, sess, "account", "账户", "account", nil)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	fail := func(message string) {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: message})
		s.redirect(w, r, "/app/account")
	}
	_, hash, err := s.store.GetUserByUsername(r.Context(), user.Username)
	if err != nil || !auth.VerifyPassword(hash, current) {
		fail("当前密码不正确")
		return
	}
	if len(next) < 8 {
		fail("新密码至少 8 个字符")
		return
	}
	if next != confirm {
		fail("两次输入的新密码不一致")
		return
	}
	newHash, err := auth.HashPassword(next)
	if err != nil {
		s.log.Error("生成密码哈希失败", "error", err)
		fail("修改密码失败，请稍后重试")
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), user.ID, newHash); err != nil {
		s.log.Error("更新密码失败", "error", err)
		fail("修改密码失败，请稍后重试")
		return
	}
	if _, err := s.store.DeleteUserSessions(r.Context(), user.ID, sess.TokenHash); err != nil {
		s.log.Warn("清理其他会话失败", "error", err)
	}
	s.flash.Set(sess.TokenHash, flashMessage{Kind: "success", Text: "密码已修改，其他设备上的登录已失效。"})
	s.redirect(w, r, "/app/account")
}

func hashToken(token string) string { return auth.HashToken(token) }

func constantTimeEqual(a, b string) bool { return auth.EqualToken(a, b) }

func urlQueryEscape(value string) string { return url.QueryEscape(value) }

// safeNext 只允许站内相对路径，避免登录后被重定向到外部站点。
func safeNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/app"
	}
	return next
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
