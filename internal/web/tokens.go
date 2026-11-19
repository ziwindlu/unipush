package web

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/auth"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

type tokensData struct {
	Tokens []*store.APIToken
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	tokens, err := s.store.ListAPITokens(r.Context(), user.ID)
	if err != nil {
		s.log.Error("读取 API Token 失败", "error", err)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取 API Token 失败", "")
		return
	}
	s.render(w, r, user, sess, "tokens", "API Token", "tokens", tokensData{Tokens: tokens})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "请填写 Token 用途名称，例如 ci、nas-monitor。"})
		s.redirect(w, r, "/app/tokens")
		return
	}
	if utf8.RuneCountInString(name) > 40 {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "名称不能超过 40 个字符。"})
		s.redirect(w, r, "/app/tokens")
		return
	}

	raw, hash, err := auth.NewToken(auth.APITokenPrefix)
	if err != nil {
		s.log.Error("生成 API Token 失败", "error", err)
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "生成 Token 失败，请稍后重试。"})
		s.redirect(w, r, "/app/tokens")
		return
	}
	token := &store.APIToken{
		ID:        model.NewID("tok_"),
		UserID:    user.ID,
		Name:      name,
		Prefix:    auth.TokenPrefix(raw),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateAPIToken(r.Context(), token, hash); err != nil {
		s.log.Error("保存 API Token 失败", "error", err)
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "保存 Token 失败，请稍后重试。"})
		s.redirect(w, r, "/app/tokens")
		return
	}
	s.log.Info("创建 API Token", "user", user.Username, "token_id", token.ID, "name", token.Name)
	s.flash.Set(sess.TokenHash, flashMessage{
		Kind:        "success",
		Text:        "Token 已创建。它只显示这一次，请立即复制保存。",
		Secret:      raw,
		SecretLabel: "Authorization: Bearer",
	})
	s.redirect(w, r, "/app/tokens")
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	id := r.PathValue("id")
	err := s.store.RevokeAPIToken(r.Context(), id, user.ID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "Token 不存在或已撤销。"})
		} else {
			s.log.Error("撤销 API Token 失败", "error", err, "token_id", id)
			s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "撤销失败，请稍后重试。"})
		}
		s.redirect(w, r, "/app/tokens")
		return
	}
	s.log.Info("撤销 API Token", "user", user.Username, "token_id", id)
	s.flash.Set(sess.TokenHash, flashMessage{Kind: "success", Text: "Token 已撤销，使用它发送消息会立即返回 401。"})
	s.redirect(w, r, "/app/tokens")
}
