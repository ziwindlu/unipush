# HTTP 接口说明（M1 + M2 路由）

所有 `/v1/*` 接口要求 `Authorization: Bearer <token>`，token 来自 `UNIPUSH_AUTH_TOKEN`（可配置多个）。错误响应体统一为：

```json
{"error": {"code": "invalid_request", "message": "人类可读的原因", "field": "出错字段（可选）"}}
```

## POST /v1/messages

接收一条消息，归一化后与各渠道投递记录一起落库，返回 `202 Accepted`。

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "磁盘告警",
    "body": "剩余空间 3%",
    "level": "error",
    "source": "nas-monitor",
    "targets": ["team"],
    "link": "https://nas.local/alert/42",
    "dedupe_key": "nas-disk-42"
  }'
```

成功响应（本次显式指定了 `targets: ["team"]`，所以未走规则）：

```json
{
  "id": "msg_3f6f...",
  "status": "queued",
  "created_at": "2026-01-01T08:00:00Z",
  "route": {"mode": "explicit", "groups": ["team"], "channels": ["dingtalk"]},
  "deliveries": [
    {"id": "dlv_...", "channel": "dingtalk", "status": "pending", "groups": ["team"]}
  ]
}
```

同时返回 `Location: /v1/messages/{id}`。不传 `targets` 时由 `config.yaml` 的 `routing` 规则决定投递渠道，`route.mode` 为 `rule` 或 `default`，详见 [routing.md](routing.md)。

字段约束：

- `title` 与 `body` 至少一个非空；`title` ≤200 字符，`body` ≤8000 字符
- `level` ∈ `info`（缺省）/ `warn` / `error`
- `source` 供路由规则匹配（≤64 字符）
- `targets` 是显式投递目标：渠道组名（如 `phone` / `team`）或渠道名，只能引用启动时已启用的组/渠道；省略则按路由规则决定
- `link` 必须是 http(s) 绝对地址
- 请求体上限由 `server.max_body_bytes` 控制（默认 1 MiB）

状态码：

| 状态码 | 含义 |
| --- | --- |
| 202 | 已入队 |
| 400 | JSON 非法、字段缺失或取值非法、`targets` 里有未知的组/渠道（`error.message` 列出可用值） |
| 401 | token 缺失或无效 |
| 413 | 请求体超过上限 |
| 503 | 没有任何启用渠道；或命中的规则/组没有可用渠道（`no_route`，`error.message` 说明是哪条规则/哪个组） |
| 500 | 落库失败等内部错误 |

## GET /v1/messages/{id}

```bash
curl -sS http://127.0.0.1:8080/v1/messages/msg_3f6f... \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"
```

下面这条 `ci` 来源的消息没有传 `targets`，由规则 `ci-build` 投到组 `team`：

```json
{
  "message": {
    "id": "msg_3f6f...",
    "title": "构建完成",
    "body": "v1.2.3 已上线",
    "level": "info",
    "source": "ci",
    "route": {
      "mode": "rule",
      "rule": "ci-build",
      "groups": ["team"],
      "channels": ["dingtalk"]
    },
    "created_at": "2026-01-01T08:00:00Z"
  },
  "status": "succeeded",
  "deliveries": [
    {
      "id": "dlv_...",
      "message_id": "msg_3f6f...",
      "channel": "dingtalk",
      "status": "succeeded",
      "attempts": 1,
      "max_attempts": 8,
      "rule": "ci-build",
      "groups": ["team"],
      "last_attempt_at": "2026-01-01T08:00:00Z",
      "succeeded_at": "2026-01-01T08:00:00Z",
      "created_at": "2026-01-01T08:00:00Z",
      "updated_at": "2026-01-01T08:00:00Z"
    }
  ]
}
```

聚合 `status`：

| 值 | 含义 |
| --- | --- |
| `queued` | 没有投递记录（理论边界） |
| `in_progress` | 至少一个渠道仍在排队或发送中 |
| `succeeded` | 所有渠道成功 |
| `partial` | 部分成功、部分最终失败 |
| `failed` | 所有渠道最终失败 |

单渠道 `status`：`pending`（等待/退避中）、`sending`（已认领）、`succeeded`、`failed`。失败原因在 `last_error`，下次尝试时间在 `next_attempt_at`。

`message.route` 是路由解析结果：

| 字段 | 含义 |
| --- | --- |
| `mode` | `explicit`（调用方指定 targets）/ `rule`（命中规则）/ `default`（默认目标） |
| `rule` | 命中的规则名；默认目标为 `default`；显式指定时省略 |
| `groups` | 命中的渠道组（按配置顺序）；显式指定渠道名时为空 |
| `channels` | 最终投递的渠道（去重排序） |

每条 delivery 的 `rule` / `groups` 说明该渠道是被哪条规则、哪些组带进来的；fan-out 时 `groups` 可能有多个。升级前的老消息没有 `route` 字段。

不存在返回 404。

## GET /healthz

不需要鉴权，检查进程与 SQLite 连通性：

```json
{"status": "ok"}
```

数据库不可用时返回 `503` 与 `{"status": "unavailable", "error": "..."}`。
