package web

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ziwindlu/unipush/internal/auth"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

type usersData struct {
	Users         []*store.User
	CurrentUserID string
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.log.Error("读取用户列表失败", "error", err)
		s.renderError(w, r, user, sess, http.StatusInternalServerError, "读取用户列表失败", "")
		return
	}
	s.render(w, r, user, sess, "users", "用户", "users", usersData{Users: users, CurrentUserID: user.ID})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	displayName := strings.TrimSpace(r.PostFormValue("display_name"))
	role := strings.ToLower(strings.TrimSpace(r.PostFormValue("role")))
	password := r.PostFormValue("password")

	fail := func(message string) {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: message})
		s.redirect(w, r, "/app/users")
	}
	if !usernamePattern.MatchString(username) {
		fail("用户名需为 3-32 位小写字母、数字或 . _ -，且以字母或数字开头。")
		return
	}
	if utf8.RuneCountInString(displayName) > 40 {
		fail("显示名不能超过 40 个字符。")
		return
	}
	if role == "" {
		role = store.RoleMember
	}
	if !store.ValidRole(role) {
		fail("角色只能是 admin 或 member。")
		return
	}

	generated := ""
	if password == "" {
		var err error
		password, err = auth.GeneratePassword(14)
		if err != nil {
			s.log.Error("生成初始密码失败", "error", err)
			fail("创建用户失败，请稍后重试。")
			return
		}
		generated = password
	}
	if len(password) < 8 {
		fail("密码至少 8 个字符。")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.log.Error("生成密码哈希失败", "error", err)
		fail("创建用户失败，请稍后重试。")
		return
	}
	newUser := &store.User{
		ID:          model.NewID("usr_"),
		Username:    username,
		DisplayName: displayName,
		Role:        role,
	}
	if err := s.store.CreateUser(r.Context(), newUser, hash); err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			fail("用户名 " + username + " 已存在。")
			return
		}
		s.log.Error("创建用户失败", "error", err)
		fail("创建用户失败，请查看服务日志。")
		return
	}
	s.log.Info("创建 Web 用户", "operator", user.Username, "username", username, "role", role)
	msg := flashMessage{Kind: "success", Text: "用户 " + username + " 已创建。"}
	if generated != "" {
		msg.Text = "用户 " + username + " 已创建，请把初始密码转交给对方。"
		msg.Secret = generated
		msg.SecretLabel = "初始密码"
	}
	s.flash.Set(sess.TokenHash, msg)
	s.redirect(w, r, "/app/users")
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	id := r.PathValue("id")
	target, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "用户不存在。"})
		s.redirect(w, r, "/app/users")
		return
	}
	password, err := auth.GeneratePassword(14)
	if err != nil {
		s.log.Error("生成随机密码失败", "error", err)
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "重置密码失败，请稍后重试。"})
		s.redirect(w, r, "/app/users")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.log.Error("生成密码哈希失败", "error", err)
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "重置密码失败，请稍后重试。"})
		s.redirect(w, r, "/app/users")
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), id, hash); err != nil {
		s.log.Error("重置用户密码失败", "error", err)
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "重置密码失败，请稍后重试。"})
		s.redirect(w, r, "/app/users")
		return
	}
	// 管理员重置自己的密码时保留当前会话，否则会被立即踢回登录页。
	except := ""
	if id == user.ID {
		except = sess.TokenHash
	}
	if _, err := s.store.DeleteUserSessions(r.Context(), id, except); err != nil {
		s.log.Warn("清理被重置用户的会话失败", "error", err)
	}
	s.log.Info("重置 Web 用户密码", "operator", user.Username, "username", target.Username)
	s.flash.Set(sess.TokenHash, flashMessage{
		Kind:        "success",
		Text:        "已重置 " + target.Username + " 的密码，该用户所有登录会话已失效。",
		Secret:      password,
		SecretLabel: "新密码",
	})
	s.redirect(w, r, "/app/users")
}

func (s *Server) handleUserStatus(w http.ResponseWriter, r *http.Request, user *store.User, sess *store.Session) {
	if !s.checkCSRF(w, r, user, sess) {
		return
	}
	id := r.PathValue("id")
	action := strings.ToLower(strings.TrimSpace(r.PostFormValue("action")))
	target, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "用户不存在。"})
		s.redirect(w, r, "/app/users")
		return
	}
	if action != "disable" && action != "enable" {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: "不支持的操作。"})
		s.redirect(w, r, "/app/users")
		return
	}
	disable := action == "disable"
	fail := func(message string) {
		s.flash.Set(sess.TokenHash, flashMessage{Kind: "error", Text: message})
		s.redirect(w, r, "/app/users")
	}
	if disable && target.ID == user.ID {
		fail("不能停用当前登录的账号。")
		return
	}
	if disable && target.Role == store.RoleAdmin {
		admins, err := s.store.CountActiveAdmins(r.Context())
		if err != nil {
			s.log.Error("统计管理员失败", "error", err)
			fail("操作失败，请稍后重试。")
			return
		}
		if admins <= 1 {
			fail("至少需要保留一个启用的管理员。")
			return
		}
	}
	if err := s.store.SetUserDisabled(r.Context(), id, disable); err != nil {
		s.log.Error("更新用户状态失败", "error", err)
		fail("操作失败，请稍后重试。")
		return
	}
	s.log.Info("更新 Web 用户状态", "operator", user.Username, "username", target.Username, "disabled", disable)
	state := "已启用"
	if disable {
		state = "已停用，所有会话与 API Token 立即失效"
	}
	s.flash.Set(sess.TokenHash, flashMessage{Kind: "success", Text: "用户 " + target.Username + " " + state + "。"})
	s.redirect(w, r, "/app/users")
}
