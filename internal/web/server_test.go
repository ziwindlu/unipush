package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ziwindlu/unipush/internal/api"
	"github.com/ziwindlu/unipush/internal/auth"
	"github.com/ziwindlu/unipush/internal/model"
	"github.com/ziwindlu/unipush/internal/store"
)

var (
	adminPassword = strings.Repeat("a", 16)
	alicePassword = strings.Repeat("b", 16)
	newPassword   = strings.Repeat("c", 16)
)

type fakeQueue struct {
	channels []string
	groups   []string
	enqueued []*model.Message
	result   *model.EnqueueResult
	err      error
}

func (q *fakeQueue) Channels() []string { return q.channels }
func (q *fakeQueue) Groups() []string   { return q.groups }

func (q *fakeQueue) Enqueue(_ context.Context, msg *model.Message) (*model.EnqueueResult, error) {
	if q.err != nil {
		return nil, q.err
	}
	q.enqueued = append(q.enqueued, msg)
	if q.result != nil {
		return q.result, nil
	}
	return &model.EnqueueResult{MessageID: msg.ID, Status: model.EnqueueQueued}, nil
}

type fixture struct {
	store *store.Store
	queue *fakeQueue
	web   *Server
	http  *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := BootstrapAdmin(context.Background(), st, "admin", adminPassword, logger); err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	queue := &fakeQueue{
		channels: []string{"bark", "dingtalk"},
		groups:   []string{"phone", "team"},
	}
	webServer, err := New(Config{Version: "test", SessionTTL: time.Hour}, st, queue, logger)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	apiServer := api.New(api.Config{
		AuthTokens: []string{"static-token"},
		TokenVerifier: func(ctx context.Context, token string) (bool, error) {
			_, _, err := st.VerifyAPIToken(ctx, auth.HashToken(token), time.Now().UTC())
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return true, nil
		},
	}, st, queue, logger)
	root := http.NewServeMux()
	root.Handle("/v1/", apiServer.Handler())
	root.Handle("/healthz", apiServer.Handler())
	root.Handle("/", webServer.Handler())
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return &fixture{store: st, queue: queue, web: webServer, http: srv}
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func formPost(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(target, values)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func get(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func bodyText(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	return string(raw)
}

var (
	csrfPattern  = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	tokenPattern = regexp.MustCompile(`up_[A-Za-z0-9_-]{20,}`)
)

func login(t *testing.T, base, username, password string) (*http.Client, string) {
	t.Helper()
	client := newClient(t)
	resp := formPost(t, client, base+"/login", url.Values{
		"username": {username},
		"password": {password},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录状态 = %d, 期望 303；响应: %s", resp.StatusCode, bodyText(t, resp))
	}
	if loc := resp.Header.Get("Location"); loc != "/app" {
		t.Fatalf("登录后跳转 = %q, 期望 /app", loc)
	}
	dashboard := get(t, client, base+"/app")
	if dashboard.StatusCode != http.StatusOK {
		t.Fatalf("概览页状态 = %d", dashboard.StatusCode)
	}
	body := bodyText(t, dashboard)
	if !strings.Contains(body, "概览") {
		t.Fatalf("概览页缺少标题: %s", body[:min(len(body), 300)])
	}
	match := csrfPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("概览页没有 csrf_token")
	}
	return client, match[1]
}

func csrfFrom(t *testing.T, client *http.Client, target string) (string, string) {
	t.Helper()
	resp := get(t, client, target)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", target, resp.StatusCode)
	}
	body := bodyText(t, resp)
	match := csrfPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("%s 没有 csrf_token", target)
	}
	return match[1], body
}

func TestLoginSecurityAndHeaders(t *testing.T) {
	fx := newFixture(t)
	client := newClient(t)

	resp := get(t, client, fx.http.URL+"/app")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("未登录访问 = %d %q, 期望跳转登录", resp.StatusCode, resp.Header.Get("Location"))
	}
	loginPage := get(t, client, fx.http.URL+"/login")
	body := bodyText(t, loginPage)
	if !strings.Contains(body, "登录") || !strings.Contains(body, "unipush") {
		t.Fatal("登录页内容不完整")
	}
	if csp := loginPage.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("缺少 CSP 响应头: %q", csp)
	}

	css := get(t, client, fx.http.URL+"/static/app.css")
	if css.StatusCode != http.StatusOK {
		t.Fatalf("静态资源状态 = %d", css.StatusCode)
	}

	wrong := newClient(t)
	bad := formPost(t, wrong, fx.http.URL+"/login", url.Values{
		"username": {"admin"},
		"password": {"wrong-password"},
	})
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密码状态 = %d, 期望 401", bad.StatusCode)
	}
	if text := bodyText(t, bad); !strings.Contains(text, "用户名或密码错误") {
		t.Fatalf("错误提示不明确: %s", text)
	}

	good := newClient(t)
	ok := formPost(t, good, fx.http.URL+"/login", url.Values{
		"username": {"admin"},
		"password": {adminPassword},
	})
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("正确密码状态 = %d", ok.StatusCode)
	}
	setCookie := ok.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Lax") {
		t.Fatalf("会话 Cookie 缺少安全属性: %q", setCookie)
	}
}

func TestCSRFRequiredForWrites(t *testing.T) {
	fx := newFixture(t)
	client, _ := login(t, fx.http.URL, "admin", adminPassword)

	resp := formPost(t, client, fx.http.URL+"/app/tokens", url.Values{"name": {"no-csrf"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 的状态 = %d, 期望 403", resp.StatusCode)
	}
	if len(fx.storeTokens(t)) != 0 {
		t.Fatal("CSRF 校验失败时不应创建 Token")
	}
}

func (fx *fixture) storeTokens(t *testing.T) []*store.APIToken {
	t.Helper()
	users, err := fx.store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var tokens []*store.APIToken
	for _, user := range users {
		list, err := fx.store.ListAPITokens(context.Background(), user.ID)
		if err != nil {
			t.Fatalf("ListAPITokens: %v", err)
		}
		tokens = append(tokens, list...)
	}
	return tokens
}

func TestCreateTokenAndUseItOnAPI(t *testing.T) {
	fx := newFixture(t)
	client, csrf := login(t, fx.http.URL, "admin", adminPassword)

	resp := formPost(t, client, fx.http.URL+"/app/tokens", url.Values{
		"csrf_token": {csrf},
		"name":       {"ci-build"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("创建 Token 状态 = %d", resp.StatusCode)
	}
	page := get(t, client, fx.http.URL+"/app/tokens")
	body := bodyText(t, page)
	if !strings.Contains(body, "只显示这一次") {
		t.Fatalf("创建后没有展示一次性提示: %s", body)
	}
	match := tokenPattern.FindString(body)
	if match == "" {
		t.Fatalf("页面里找不到新 Token: %s", body)
	}

	payload := strings.NewReader(`{"title":"部署完成","body":"v1 上线","level":"warn","source":"ci"}`)
	req, _ := http.NewRequest(http.MethodPost, fx.http.URL+"/v1/messages", payload)
	req.Header.Set("Authorization", "Bearer "+match)
	req.Header.Set("Content-Type", "application/json")
	apiResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("调用 API: %v", err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusAccepted {
		t.Fatalf("动态 Token 调用 API 状态 = %d, 期望 202；响应: %s", apiResp.StatusCode, bodyText(t, apiResp))
	}
	if len(fx.queue.enqueued) != 1 {
		t.Fatalf("入队消息数 = %d, 期望 1", len(fx.queue.enqueued))
	}

	csrf, _ = csrfFrom(t, client, fx.http.URL+"/app/tokens")
	tokens := fx.storeTokens(t)
	if len(tokens) != 1 {
		t.Fatalf("Token 数 = %d, 期望 1", len(tokens))
	}
	revoke := formPost(t, client, fx.http.URL+"/app/tokens/"+tokens[0].ID+"/revoke", url.Values{
		"csrf_token": {csrf},
	})
	if revoke.StatusCode != http.StatusSeeOther {
		t.Fatalf("撤销状态 = %d", revoke.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, fx.http.URL+"/v1/messages", strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Authorization", "Bearer "+match)
	req.Header.Set("Content-Type", "application/json")
	apiResp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("撤销后调用 API: %v", err)
	}
	defer apiResp2.Body.Close()
	if apiResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("撤销后 API 状态 = %d, 期望 401", apiResp2.StatusCode)
	}
}

func TestSendMessageFromWeb(t *testing.T) {
	fx := newFixture(t)
	client, csrf := login(t, fx.http.URL, "admin", adminPassword)

	resp := formPost(t, client, fx.http.URL+"/app/send", url.Values{
		"csrf_token": {csrf},
		"title":      {"备份完成"},
		"body":       {"3 个文件，共 1.2 GB"},
		"level":      {"warn"},
		"source":     {"nas-monitor"},
		"targets":    {"phone"},
		"dedupe_key": {"backup-1"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("发送状态 = %d；响应: %s", resp.StatusCode, bodyText(t, resp))
	}
	if !strings.HasPrefix(resp.Header.Get("Location"), "/app/messages/msg_") {
		t.Fatalf("发送后跳转 = %q", resp.Header.Get("Location"))
	}
	if len(fx.queue.enqueued) != 1 {
		t.Fatalf("入队消息数 = %d, 期望 1", len(fx.queue.enqueued))
	}
	msg := fx.queue.enqueued[0]
	if msg.Title != "备份完成" || msg.Level != model.LevelWarn || msg.Source != "nas-monitor" {
		t.Fatalf("消息字段不符: %+v", msg)
	}
	if len(msg.Targets) != 1 || msg.Targets[0] != "phone" {
		t.Fatalf("targets = %v, 期望 [phone]", msg.Targets)
	}
}

func TestSendValidationErrorKeepsForm(t *testing.T) {
	fx := newFixture(t)
	client, csrf := login(t, fx.http.URL, "admin", adminPassword)

	resp := formPost(t, client, fx.http.URL+"/app/send", url.Values{
		"csrf_token": {csrf},
		"title":      {""},
		"body":       {""},
		"level":      {"warn"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空消息状态 = %d, 期望 400", resp.StatusCode)
	}
	if body := bodyText(t, resp); !strings.Contains(body, "不能同时为空") {
		t.Fatalf("错误提示不符合预期: %s", body)
	}
	if len(fx.queue.enqueued) != 0 {
		t.Fatal("校验失败时不应入队")
	}
}

func TestMessageListAndDetailRender(t *testing.T) {
	fx := newFixture(t)
	now := time.Now().UTC()
	msg := &model.Message{
		ID:        "msg_render",
		Title:     "渲染测试",
		Body:      "第一行\n第二行",
		Level:     model.LevelWarn,
		Source:    "ci",
		CreatedAt: now,
		Route:     &model.Route{Mode: "rule", Rule: "ci-build", Groups: []string{"team"}, Channels: []string{"dingtalk"}},
	}
	delivery := &model.Delivery{
		ID:          "dlv_render",
		MessageID:   msg.ID,
		Channel:     "dingtalk",
		Status:      model.DeliveryFailed,
		Attempts:    2,
		MaxAttempts: 8,
		Rule:        "ci-build",
		Groups:      []string{"team"},
		LastError:   "关键词不匹配",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := fx.store.Enqueue(context.Background(), msg, []*model.Delivery{delivery}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	client, _ := login(t, fx.http.URL, "admin", adminPassword)
	list := get(t, client, fx.http.URL+"/app/messages")
	body := bodyText(t, list)
	if list.StatusCode != http.StatusOK || !strings.Contains(body, "渲染测试") || !strings.Contains(body, "dingtalk") {
		t.Fatalf("消息列表渲染异常 (%d): %s", list.StatusCode, body)
	}

	detail := get(t, client, fx.http.URL+"/app/messages/msg_render")
	detailBody := bodyText(t, detail)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("详情状态 = %d: %s", detail.StatusCode, detailBody)
	}
	for _, want := range []string{"渲染测试", "投递明细", "关键词不匹配", "ci-build", "失败"} {
		if !strings.Contains(detailBody, want) {
			t.Fatalf("详情页缺少 %q", want)
		}
	}

	bad := get(t, client, fx.http.URL+"/app/messages?level=urgent")
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法过滤状态 = %d, 期望 400", bad.StatusCode)
	}
}

func TestAdminCreatesUserAndMemberCannotManage(t *testing.T) {
	fx := newFixture(t)
	admin, csrf := login(t, fx.http.URL, "admin", adminPassword)

	resp := formPost(t, admin, fx.http.URL+"/app/users", url.Values{
		"csrf_token": {"bad-token"},
		"username":   {"alice"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("错误 CSRF 状态 = %d, 期望 403", resp.StatusCode)
	}

	resp = formPost(t, admin, fx.http.URL+"/app/users", url.Values{
		"csrf_token":   {csrf},
		"username":     {"alice"},
		"display_name": {"Alice"},
		"role":         {"member"},
		"password":     {alicePassword},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("创建用户状态 = %d；响应: %s", resp.StatusCode, bodyText(t, resp))
	}
	users := get(t, admin, fx.http.URL+"/app/users")
	if body := bodyText(t, users); !strings.Contains(body, "alice") || !strings.Contains(body, "Alice") {
		t.Fatalf("用户列表缺少新用户: %s", body)
	}

	alice, _ := login(t, fx.http.URL, "alice", alicePassword)
	forbidden := get(t, alice, fx.http.URL+"/app/users")
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("普通成员访问用户管理状态 = %d, 期望 403", forbidden.StatusCode)
	}
	if body := bodyText(t, forbidden); !strings.Contains(body, "需要管理员权限") {
		t.Fatalf("缺少权限提示: %s", body)
	}
}

func TestCannotDisableSelf(t *testing.T) {
	fx := newFixture(t)
	client, csrf := login(t, fx.http.URL, "admin", adminPassword)
	users, err := fx.store.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers: %v, %d", err, len(users))
	}
	resp := formPost(t, client, fx.http.URL+"/app/users/"+users[0].ID+"/status", url.Values{
		"csrf_token": {csrf},
		"action":     {"disable"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("停用自己状态 = %d", resp.StatusCode)
	}
	page := get(t, client, fx.http.URL+"/app/users")
	if body := bodyText(t, page); !strings.Contains(body, "不能停用当前登录的账号") {
		t.Fatalf("缺少保护提示: %s", body)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	fx := newFixture(t)
	sessionA, _ := login(t, fx.http.URL, "admin", adminPassword)
	sessionB, _ := login(t, fx.http.URL, "admin", adminPassword)

	csrf, _ := csrfFrom(t, sessionA, fx.http.URL+"/app/account")
	resp := formPost(t, sessionA, fx.http.URL+"/app/account/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {adminPassword},
		"new_password":     {newPassword},
		"confirm_password": {newPassword},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("修改密码状态 = %d", resp.StatusCode)
	}
	if page := get(t, sessionA, fx.http.URL+"/app"); page.StatusCode != http.StatusOK {
		t.Fatalf("当前会话被误杀: %d", page.StatusCode)
	}
	other := get(t, sessionB, fx.http.URL+"/app")
	if other.StatusCode != http.StatusSeeOther || !strings.HasPrefix(other.Header.Get("Location"), "/login") {
		t.Fatalf("其他会话状态 = %d %q, 期望跳转登录", other.StatusCode, other.Header.Get("Location"))
	}
}

func TestBootstrapAdminIsIdempotent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	generated, err := BootstrapAdmin(context.Background(), st, "admin", "", logger)
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if len(generated) < 8 {
		t.Fatalf("随机初始密码太短: %q", generated)
	}
	again, err := BootstrapAdmin(context.Background(), st, "other", "other-password", logger)
	if err != nil {
		t.Fatalf("第二次 BootstrapAdmin: %v", err)
	}
	if again != "" {
		t.Fatal("已有用户时不应再创建管理员")
	}
	users, err := st.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("用户数 = %d, 期望 1", len(users))
	}
}

func TestLoginLimiterWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(3, time.Minute)
	limiter.now = func() time.Time { return now }
	key := "127.0.0.1|admin"
	for i := 0; i < 3; i++ {
		if !limiter.Allow(key) {
			t.Fatalf("第 %d 次尝试应被允许", i+1)
		}
		limiter.Fail(key)
	}
	if limiter.Allow(key) {
		t.Fatal("连续失败 3 次后应暂时拒绝")
	}
	now = now.Add(61 * time.Second)
	if !limiter.Allow(key) {
		t.Fatal("窗口过期后应重新允许")
	}
}
