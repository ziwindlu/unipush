package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runCLI 用干净的环境变量执行一次 CLI，避免读到开发机上的 .env / token。
func runCLI(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("UNIPUSH_URL", "")
	t.Setenv("UNIPUSH_AUTH_TOKEN", "")
	var out, errOut bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSendSuccess(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var payload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"msg_123","status":"queued","route":{"mode":"rule","rule":"nas-alerts","groups":["phone"],"channels":["bark"]},"deliveries":[{"id":"dlv_1","channel":"bark","status":"pending"}]}`))
	}))
	defer ts.Close()

	code, stdout, stderr := runCLI(t, "",
		"send", "--url", ts.URL, "--token", "tok", "--title", "磁盘告警", "--body", "剩余 3%",
		"--level", "warn", "--source", "nas-monitor", "--targets", "phone,team",
		"--dedupe-key", "disk-42", "--link", "https://nas.local/1", "--env-file", "")
	if code != 0 {
		t.Fatalf("退出码 = %d, stderr=%q", code, stderr)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Errorf("请求 = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if payload["title"] != "磁盘告警" || payload["level"] != "warn" || payload["source"] != "nas-monitor" ||
		payload["dedupe_key"] != "disk-42" || payload["link"] != "https://nas.local/1" {
		t.Errorf("请求体字段不对: %#v", payload)
	}
	targets, _ := payload["targets"].([]any)
	if len(targets) != 2 || targets[0] != "phone" || targets[1] != "team" {
		t.Errorf("targets = %#v", payload["targets"])
	}
	if !strings.Contains(stdout, "msg_123 已入队") || !strings.Contains(stdout, "规则 nas-alerts") {
		t.Errorf("stdout 摘要不对: %q", stdout)
	}
}

func TestSendReadsStdinAndQuiet(t *testing.T) {
	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		body, _ = payload["body"].(string)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"msg_stdin","status":"queued","deliveries":[]}`))
	}))
	defer ts.Close()

	code, stdout, stderr := runCLI(t, "来自脚本的输出\n", "send", "--url", ts.URL, "--token", "tok",
		"--title", "巡检", "--body", "-", "--quiet", "--env-file", "")
	if code != 0 {
		t.Fatalf("退出码 = %d, stderr=%q", code, stderr)
	}
	if body != "来自脚本的输出\n" {
		t.Errorf("stdin 正文 = %q", body)
	}
	if strings.TrimSpace(stdout) != "msg_stdin" {
		t.Errorf("--quiet 应只输出 ID，实际 %q", stdout)
	}
}

func TestSendJSONOutput(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"msg_json","status":"aggregated","deliveries":[]}`))
	}))
	defer ts.Close()

	code, stdout, _ := runCLI(t, "", "send", "--url", strings.TrimPrefix(ts.URL, "http://"), "--token", "tok",
		"--title", "t", "--json", "--env-file", "")
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(stdout, `"id": "msg_json"`) {
		t.Errorf("--json 应输出缩进 JSON，实际 %q", stdout)
	}
}

func TestSendDedupedSummary(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"msg_dup","status":"deduped","merged":2,"deliveries":[]}`))
	}))
	defer ts.Close()

	code, stdout, _ := runCLI(t, "", "send", "--url", ts.URL, "--token", "tok", "--title", "t", "--env-file", "")
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(stdout, "已合并") || !strings.Contains(stdout, "第 3 次") {
		t.Errorf("去重摘要不对: %q", stdout)
	}
}

func TestSendServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"invalid_request","message":"level 必须是 info / warn / error 之一","field":"level"}}`))
	}))
	defer ts.Close()

	code, _, stderr := runCLI(t, "", "send", "--url", ts.URL, "--token", "tok", "--title", "t", "--env-file", "")
	if code != 1 {
		t.Fatalf("退出码 = %d, 期望 1", code)
	}
	for _, want := range []string{"HTTP 400", "level 必须是", "字段 level"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr 应包含 %q，实际 %q", want, stderr)
		}
	}
}

func TestSendValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing token", []string{"send", "--title", "t", "--env-file", ""}, "缺少 token"},
		{"missing content", []string{"send", "--token", "tok", "--env-file", ""}, "title 与 body 不能同时为空"},
		{"bad level", []string{"send", "--token", "tok", "--title", "t", "--level", "fatal", "--env-file", ""}, "level 必须是"},
		{"positional", []string{"send", "--token", "tok", "--title", "t", "extra", "--env-file", ""}, "不接受位置参数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, "", tc.args...)
			if code != 2 {
				t.Fatalf("退出码 = %d, 期望 2（stderr=%q）", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, 期望包含 %q", stderr, tc.want)
			}
		})
	}
}

func TestSendEmptyTitleAllowedWithBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"msg_bodyonly","status":"queued"}`))
	}))
	defer ts.Close()
	code, _, stderr := runCLI(t, "", "send", "--url", ts.URL, "--token", "tok", "--body", "只有正文", "--env-file", "")
	if code != 0 {
		t.Fatalf("退出码 = %d, stderr=%q", code, stderr)
	}
}

func TestRunSubcommands(t *testing.T) {
	if code, out, _ := runCLI(t, "", "version"); code != 0 || !strings.Contains(out, "push") {
		t.Errorf("version: code=%d out=%q", code, out)
	}
	if code, _, _ := runCLI(t, ""); code != 2 {
		t.Errorf("无参数退出码 = %d, 期望 2", code)
	}
	if code, _, stderr := runCLI(t, "", "publish"); code != 2 || !strings.Contains(stderr, "未知子命令") {
		t.Errorf("未知子命令: code=%d stderr=%q", code, stderr)
	}
	if code, out, _ := runCLI(t, "", "help"); code != 0 || !strings.Contains(out, "push send") {
		t.Errorf("help: code=%d out=%q", code, out)
	}
}
