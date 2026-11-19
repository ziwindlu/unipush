package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 用户角色。admin 可以管理用户，member 只能使用自己的 Token 与消息功能。
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ValidRole 报告角色是否受支持。
func ValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleMember:
		return true
	default:
		return false
	}
}

// ErrUsernameTaken 表示用户名已被占用。
var ErrUsernameTaken = errors.New("用户名已存在")

// ErrConflict 表示操作会破坏系统约束（例如停用最后一个管理员）。
var ErrConflict = errors.New("操作与当前状态冲突")

// User 是 Web 控制台的登录用户。
type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Role        string     `json:"role"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// IsAdmin 报告用户是否有管理权限。
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// Session 是一次已登录的浏览器会话。
type Session struct {
	TokenHash string
	CSRFToken string
	ExpiresAt time.Time
}

// APIToken 是用户创建的、用于调用 POST /v1/messages 的 Token。
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreateUser 创建用户；用户名重复时返回 ErrUsernameTaken。
func (s *Store) CreateUser(ctx context.Context, user *User, passwordHash string) error {
	if user.ID == "" || user.Username == "" || passwordHash == "" {
		return fmt.Errorf("store: 创建用户缺少必要字段")
	}
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO users
		(id, username, display_name, password_hash, role, disabled, created_at, updated_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.DisplayName, passwordHash, user.Role, boolInt(user.Disabled),
		user.CreatedAt.UnixMilli(), user.UpdatedAt.UnixMilli(), millisOf(user.LastLoginAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: users.username") {
			return ErrUsernameTaken
		}
		return fmt.Errorf("store: 创建用户: %w", err)
	}
	return nil
}

// GetUserByUsername 读取用户及其密码哈希，供登录校验使用。
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+`, password_hash
		FROM users WHERE username = ?`, username)
	return scanUserWithHash(row)
}

// GetUser 按 ID 读取用户。
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	user, err := scanUser(row)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// ListUsers 返回全部用户（不含密码哈希）。
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+`
		FROM users ORDER BY created_at, username`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户列表: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历用户列表: %w", err)
	}
	return users, nil
}

// CountUsers 返回用户总数，用于判断是否需要创建初始管理员。
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: 统计用户: %w", err)
	}
	return count, nil
}

// CountActiveAdmins 返回未停用的管理员数量。
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users
		WHERE role = ? AND disabled = 0`, RoleAdmin).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: 统计管理员: %w", err)
	}
	return count, nil
}

// UpdateUserPassword 更新密码哈希；调用方负责按需清理该用户的会话。
func (s *Store) UpdateUserPassword(ctx context.Context, id, passwordHash string) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: 更新密码: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetUserDisabled 启用或停用用户；停用会同时删除其全部会话。
func (s *Store) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE users SET disabled = ?, updated_at = ? WHERE id = ?`,
		boolInt(disabled), now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: 更新用户状态: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if disabled {
		if _, err := s.DeleteUserSessions(ctx, id, ""); err != nil {
			return err
		}
	}
	return nil
}

// TouchUserLogin 记录最近一次登录时间。
func (s *Store) TouchUserLogin(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ?`,
		at.UTC().UnixMilli(), at.UTC().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: 记录登录时间: %w", err)
	}
	return nil
}

// CreateSession 保存浏览器会话。数据库只保存 token 哈希。
func (s *Store) CreateSession(ctx context.Context, tokenHash, userID, csrfToken string, now, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(token_hash, user_id, csrf_token, created_at, expires_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, csrfToken, now.UTC().UnixMilli(), expiresAt.UTC().UnixMilli(), now.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: 创建会话: %w", err)
	}
	return nil
}

// GetSession 校验会话并返回对应用户；过期、已停用用户或不存在都返回 ErrNotFound。
func (s *Store) GetSession(ctx context.Context, tokenHash string, now time.Time) (*User, *Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumnsAliased+`,
		sess.csrf_token, sess.expires_at, sess.last_seen_at
		FROM sessions sess JOIN users u ON u.id = sess.user_id
		WHERE sess.token_hash = ? AND sess.expires_at > ? AND u.disabled = 0`,
		tokenHash, now.UTC().UnixMilli())
	user := &User{}
	var (
		csrf       string
		expiresAt  int64
		lastSeenAt int64
		lastLogin  sql.NullInt64
		disabled   int
		createdAt  int64
		updatedAt  int64
	)
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &disabled,
		&createdAt, &updatedAt, &lastLogin, &csrf, &expiresAt, &lastSeenAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("store: 读取会话: %w", err)
	}
	user.Disabled = disabled != 0
	user.CreatedAt = time.UnixMilli(createdAt).UTC()
	user.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	user.LastLoginAt = timePtr(lastLogin)

	// 每 5 分钟刷新一次活跃时间，避免每个请求都写库。
	if now.Sub(time.UnixMilli(lastSeenAt)) >= 5*time.Minute {
		// 活跃时间只是辅助信息，写失败不应让用户掉线。
		_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
			now.UTC().UnixMilli(), tokenHash)
	}
	return user, &Session{
		TokenHash: tokenHash,
		CSRFToken: csrf,
		ExpiresAt: time.UnixMilli(expiresAt).UTC(),
	}, nil
}

// DeleteSession 删除单个会话。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("store: 删除会话: %w", err)
	}
	return nil
}

// DeleteUserSessions 删除用户的会话；exceptHash 非空时保留当前会话。
func (s *Store) DeleteUserSessions(ctx context.Context, userID, exceptHash string) (int64, error) {
	query := `DELETE FROM sessions WHERE user_id = ?`
	args := []any{userID}
	if exceptHash != "" {
		query += ` AND token_hash <> ?`
		args = append(args, exceptHash)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: 删除用户会话: %w", err)
	}
	return res.RowsAffected()
}

// CleanupExpiredSessions 删除过期会话。
func (s *Store) CleanupExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期会话: %w", err)
	}
	return res.RowsAffected()
}

// CreateAPIToken 保存 API Token 的哈希与展示前缀。
func (s *Store) CreateAPIToken(ctx context.Context, token *APIToken, tokenHash string) error {
	if token.ID == "" || token.UserID == "" || token.Name == "" || tokenHash == "" {
		return fmt.Errorf("store: 创建 API Token 缺少必要字段")
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens
		(id, user_id, name, token_hash, prefix, created_at, last_used_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		token.ID, token.UserID, token.Name, tokenHash, token.Prefix,
		token.CreatedAt.UnixMilli(), millisOf(token.LastUsedAt), millisOf(token.RevokedAt))
	if err != nil {
		return fmt.Errorf("store: 创建 API Token: %w", err)
	}
	return nil
}

// ListAPITokens 返回用户的全部 Token（含已撤销），新的在前。
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, prefix, created_at, last_used_at, revoked_at
		FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 API Token: %w", err)
	}
	defer rows.Close()

	var tokens []*APIToken
	for rows.Next() {
		token := &APIToken{}
		var createdAt int64
		var lastUsed, revoked sql.NullInt64
		if err := rows.Scan(&token.ID, &token.UserID, &token.Name, &token.Prefix,
			&createdAt, &lastUsed, &revoked); err != nil {
			return nil, fmt.Errorf("store: 扫描 API Token: %w", err)
		}
		token.CreatedAt = time.UnixMilli(createdAt).UTC()
		token.LastUsedAt = timePtr(lastUsed)
		token.RevokedAt = timePtr(revoked)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 API Token: %w", err)
	}
	return tokens, nil
}

// RevokeAPIToken 撤销用户的 Token；不属于该用户时返回 ErrNotFound。
func (s *Store) RevokeAPIToken(ctx context.Context, id, userID string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		now.UTC().UnixMilli(), id, userID)
	if err != nil {
		return fmt.Errorf("store: 撤销 API Token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// VerifyAPIToken 校验 API Token，返回 Token 与所属用户。
// 已撤销 Token、已停用用户的 Token 一律无效。
func (s *Store) VerifyAPIToken(ctx context.Context, tokenHash string, now time.Time) (*APIToken, *User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT t.id, t.user_id, t.name, t.prefix, t.created_at,
		t.last_used_at, t.revoked_at,
		u.id, u.username, u.display_name, u.role, u.disabled, u.created_at, u.updated_at, u.last_login_at
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL AND u.disabled = 0`, tokenHash)

	token := &APIToken{}
	user := &User{}
	var (
		tokenCreated, userCreated, userUpdated int64
		lastUsed, revoked, lastLogin           sql.NullInt64
		disabled                               int
	)
	err := row.Scan(&token.ID, &token.UserID, &token.Name, &token.Prefix, &tokenCreated,
		&lastUsed, &revoked,
		&user.ID, &user.Username, &user.DisplayName, &user.Role, &disabled,
		&userCreated, &userUpdated, &lastLogin)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("store: 校验 API Token: %w", err)
	}
	token.CreatedAt = time.UnixMilli(tokenCreated).UTC()
	token.LastUsedAt = timePtr(lastUsed)
	token.RevokedAt = timePtr(revoked)
	user.Disabled = disabled != 0
	user.CreatedAt = time.UnixMilli(userCreated).UTC()
	user.UpdatedAt = time.UnixMilli(userUpdated).UTC()
	user.LastLoginAt = timePtr(lastLogin)

	// 每分钟最多写一次「最近使用」，避免每条消息都产生一次写事务。
	if token.LastUsedAt == nil || now.Sub(*token.LastUsedAt) >= time.Minute {
		// 记录失败不影响本次鉴权。
		_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`,
			now.UTC().UnixMilli(), token.ID)
		token.LastUsedAt = &now
	}
	return token, user, nil
}

const (
	userColumns = `id, username, display_name, role, disabled,
		created_at, updated_at, last_login_at`
	userColumnsAliased = `u.id, u.username, u.display_name, u.role, u.disabled,
		u.created_at, u.updated_at, u.last_login_at`
)

func scanUser(row scanner) (*User, error) {
	var (
		user                 = &User{}
		disabled             int
		createdAt, updatedAt int64
		lastLogin            sql.NullInt64
	)
	if err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &disabled,
		&createdAt, &updatedAt, &lastLogin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: 读取用户: %w", err)
	}
	user.Disabled = disabled != 0
	user.CreatedAt = time.UnixMilli(createdAt).UTC()
	user.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	user.LastLoginAt = timePtr(lastLogin)
	return user, nil
}

// scanUserWithHash 读取用户列及其后的 password_hash。
func scanUserWithHash(row scanner) (*User, string, error) {
	var (
		user                 = &User{}
		disabled             int
		createdAt, updatedAt int64
		lastLogin            sql.NullInt64
		hash                 string
	)
	if err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &disabled,
		&createdAt, &updatedAt, &lastLogin, &hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("store: 读取用户: %w", err)
	}
	user.Disabled = disabled != 0
	user.CreatedAt = time.UnixMilli(createdAt).UTC()
	user.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	user.LastLoginAt = timePtr(lastLogin)
	return user, hash, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
