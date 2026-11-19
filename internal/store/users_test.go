package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/auth"
)

func openUserStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func createTestUser(t *testing.T, st *Store, username, role string) *User {
	t.Helper()
	hash, err := auth.HashPassword("password-123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user := &User{ID: "usr_" + username, Username: username, DisplayName: username, Role: role}
	if err := st.CreateUser(context.Background(), user, hash); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return user
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	st := openUserStore(t)
	createTestUser(t, st, "Alice", RoleAdmin)
	// SQLite 的 NOCASE 唯一约束应禁止大小写不同的重复用户名。
	err := st.CreateUser(context.Background(), &User{ID: "usr_2", Username: "alice", Role: RoleMember}, "hash")
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("重复用户名错误 = %v, 期望 ErrUsernameTaken", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := openUserStore(t)
	user := createTestUser(t, st, "alice", RoleMember)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.CreateSession(ctx, "hash-1", user.ID, "csrf-1", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, sess, err := st.GetSession(ctx, "hash-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Username != "alice" || sess.CSRFToken != "csrf-1" {
		t.Fatalf("会话内容不符: user=%+v session=%+v", got, sess)
	}
	if _, _, err := st.GetSession(ctx, "hash-1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期会话错误 = %v, 期望 ErrNotFound", err)
	}

	if err := st.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if _, _, err := st.GetSession(ctx, "hash-1", now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("停用用户后会话错误 = %v, 期望 ErrNotFound", err)
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	st := openUserStore(t)
	user := createTestUser(t, st, "alice", RoleMember)
	ctx := context.Background()
	now := time.Now().UTC()
	raw, hash, err := auth.NewToken(auth.APITokenPrefix)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	token := &APIToken{ID: "tok_1", UserID: user.ID, Name: "ci", Prefix: auth.TokenPrefix(raw)}
	if err := st.CreateAPIToken(ctx, token, hash); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	gotToken, gotUser, err := st.VerifyAPIToken(ctx, auth.HashToken(raw), now)
	if err != nil {
		t.Fatalf("VerifyAPIToken: %v", err)
	}
	if gotToken.ID != "tok_1" || gotUser.ID != user.ID {
		t.Fatalf("token/user 不符: %+v %+v", gotToken, gotUser)
	}
	if _, _, err := st.VerifyAPIToken(ctx, auth.HashToken("up_wrong"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("错误 token = %v, 期望 ErrNotFound", err)
	}

	if err := st.RevokeAPIToken(ctx, "tok_1", user.ID, now); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, _, err := st.VerifyAPIToken(ctx, auth.HashToken(raw), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销后 token 错误 = %v, 期望 ErrNotFound", err)
	}
}

func TestAPITokenRejectedForDisabledUser(t *testing.T) {
	st := openUserStore(t)
	user := createTestUser(t, st, "alice", RoleMember)
	ctx := context.Background()
	now := time.Now().UTC()
	raw, hash, err := auth.NewToken(auth.APITokenPrefix)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := st.CreateAPIToken(ctx, &APIToken{ID: "tok_1", UserID: user.ID, Name: "ci"}, hash); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if err := st.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if _, _, err := st.VerifyAPIToken(ctx, auth.HashToken(raw), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("停用用户后 token 错误 = %v, 期望 ErrNotFound", err)
	}
}

func TestCountActiveAdmins(t *testing.T) {
	st := openUserStore(t)
	createTestUser(t, st, "admin1", RoleAdmin)
	admin2 := createTestUser(t, st, "admin2", RoleAdmin)
	createTestUser(t, st, "member", RoleMember)
	count, err := st.CountActiveAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountActiveAdmins: %v", err)
	}
	if count != 2 {
		t.Fatalf("管理员数 = %d, 期望 2", count)
	}
	if err := st.SetUserDisabled(context.Background(), admin2.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	count, err = st.CountActiveAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountActiveAdmins: %v", err)
	}
	if count != 1 {
		t.Fatalf("停用后管理员数 = %d, 期望 1", count)
	}
}
