# HTTP 接口说明（M1）

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
    "targets": ["bark", "dingtalk"],
    "link": "https://nas.local/alert/42",
    "dedupe_key": "nas-disk-42"
  }'
```

成功响应：

```json
{
  "id": "msg_3f6f...",
  "status": "queued",
  "created_at": "2026-01-01T08:00:00Z",
  "deliveries": [
    {"id": "dlv_...", "channel": "bark", "status": "pending"},
    {"id": "dlv_...", "channel": "dingtalk", "status": "pending"}
  ]
}
```

同时返回 `Location: /v1/messages/{id}`。

字段约束：

- `title` 与 `body` 至少一个非空；`title` ≤200 字符，`body` ≤8000 字符
- `level` ∈ `info`（缺省）/ `warn` / `error`
- `targets` 只能引用启动时已启用的渠道；省略则投递到全部启用渠道
- `link` 必须是 http(s) 绝对地址
- 请求体上限由 `server.max_body_bytes` 控制（默认 1 MiB）

状态码：

| 状态码 | 含义 |
| --- | --- |
| 202 | 已入队 |
| 400 | JSON 非法、字段缺失或取值非法（`error.message` 说明原因） |
| 401 | token 缺失或无效 |
| 413 | 请求体超过上限 |
| 503 | 没有任何启用的渠道，且未显式指定 targets |
| 500 | 落库失败等内部错误 |

## GET /v1/messages/{id}

```bash
curl -sS http://127.0.0.1:8080/v1/messages/msg_3f6f... \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"
```

```json
{
  "message": {
    "id": "msg_3f6f...",
    "title": "磁盘告警",
    "body": "剩余空间 3%",
    "level": "error",
    "source": "nas-monitor",
    "targets": ["bark", "dingtalk"],
    "link": "https://nas.local/alert/42",
    "created_at": "2026-01-01T08:00:00Z"
  },
  "status": "succeeded",
  "deliveries": [
    {
      "id": "dlv_...",
      "message_id": "msg_3f6f...",
      "channel": "bark",
      "status": "succeeded",
      "attempts": 1,
      "max_attempts": 8,
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

不存在返回 404。

## GET /healthz

不需要鉴权，检查进程与 SQLite 连通性：

```json
{"status": "ok"}
```

数据库不可用时返回 `503` 与 `{"status": "unavailable", "error": "..."}`。
