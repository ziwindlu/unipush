# HTTP 接口说明（M1 + M2 路由 + M3 去重聚合 + M4 列表 + M5 Web Token）

所有 `/v1/*` 接口要求 `Authorization: Bearer <token>`。token 可以来自 `config.yaml` 的 `auth.tokens`（`UNIPUSH_AUTH_TOKEN` 等），也可以在 Web 控制台的「API Token」页创建；两者权限相同。错误响应体统一为：

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

### 入队状态

`status` 不只有 `queued`：带 `dedupe_key` 的重复消息、`info` 级消息会分别返回 `deduped` / `aggregated`。
两种情况都不会立刻产生投递记录，`Location` 指向的 ID 同样可以查询。

| status | 含义 | 响应里额外字段 |
| --- | --- | --- |
| `queued` | 已落库并创建了每个渠道的投递记录 | `deliveries` |
| `deduped` | 命中去重窗口，合并到原消息，没有新投递 | `merged`（累计合并次数）、`id` 是原消息 ID |
| `aggregated` | `info` 消息进入聚合窗口，等 digest 生成后统一投递 | `id` 是自己的消息 ID，`deliveries` 为空 |

命中去重（同一 `dedupe_key` 第 2 次及以后）：

```json
{
  "id": "msg_3f6f...",
  "status": "deduped",
  "merged": 1,
  "created_at": "2026-01-01T08:00:00Z",
  "route": {"mode": "rule", "rule": "nas-alerts", "groups": ["phone"], "channels": ["bark"]},
  "deliveries": []
}
```

进入聚合窗口：

```json
{
  "id": "msg_9a1c...",
  "status": "aggregated",
  "created_at": "2026-01-01T08:01:00Z",
  "route": {"mode": "rule", "rule": "ci-build", "groups": ["team"], "channels": ["dingtalk"]},
  "deliveries": []
}
```

字段约束：

- `title` 与 `body` 至少一个非空；`title` ≤200 字符，`body` ≤8000 字符
- `level` ∈ `info`（缺省）/ `warn` / `error`
- `source` 供路由规则匹配（≤64 字符）
- `targets` 是显式投递目标：渠道组名（如 `phone` / `team`）或渠道名，只能引用启动时已启用的组/渠道；省略则按路由规则决定
- `dedupe_key`：同一 key 在 `dedupe.window` 内只投递一次，其余只累加 `merged_count`
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

## GET /v1/messages

只读消息列表，按创建时间倒序。支持过滤与游标分页，只查库、不触发重发。

```bash
# 最近 20 条
curl -sS "http://127.0.0.1:8080/v1/messages" -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"

# 按来源 + 状态过滤，只看失败
curl -sS "http://127.0.0.1:8080/v1/messages?source=nas-monitor&status=failed&limit=50" \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"
```

查询参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `limit` | 20 | 每页条数，1–200 |
| `cursor` | 无 | 上一页返回的 `next_cursor`，原样传回 |
| `source` | 无 | 精确匹配来源，≤64 字符 |
| `level` | 无 | `info` / `warn` / `error` |
| `status` | 无 | `queued` / `aggregated` / `in_progress` / `succeeded` / `partial` / `failed`（与详情接口的聚合状态一致） |
| `since` | 无 | 只返回该时间之后创建的消息，RFC3339，例如 `2026-01-01T00:00:00Z` |

响应：

```json
{
  "messages": [
    {
      "id": "msg_3f6f...",
      "title": "磁盘告警",
      "body": "剩余空间 3%",
      "level": "error",
      "source": "nas-monitor",
      "route": {"mode": "rule", "rule": "nas-alerts", "groups": ["phone"], "channels": ["bark"]},
      "merged_count": 0,
      "created_at": "2026-01-01T08:00:00Z",
      "status": "failed",
      "deliveries": [
        {"id": "dlv_...", "channel": "bark", "status": "failed", "attempts": 8,
         "last_error": "bark: HTTP 401: device key 无效"}
      ]
    }
  ],
  "next_cursor": "MTc2NzI0NDgwMDAwMDptc2dfM2Y2Zg"
}
```

- 每条消息平铺 `message` 的字段，另加聚合后的 `status` 和每渠道的投递摘要（`last_error` / `next_attempt_at` 便于直接判断卡在哪）。
- `next_cursor` 只在还有下一页时出现；把它作为 `cursor` 传入即可翻页。没有更多数据时响应里没有该字段。
- 空结果返回 `"messages": []`。
- 400：`limit` / `level` / `status` / `since` / `cursor` 非法，`error.field` 指出是哪个参数。

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
| `queued` | 还没有投递记录（理论边界） |
| `aggregated` | 这条是已汇入 digest 的原始 info 消息，本身不再有投递 |
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

`message` 上还有三个 M3 字段：

| 字段 | 含义 |
| --- | --- |
| `merged_count` | 这条消息的 `dedupe_key` 被合并掉的次数（去重命中时累加） |
| `aggregated_into` | 这条 info 消息被汇总进哪条 digest（digest 消息本身没有） |
| `digest_count` | 这条 digest 汇总了多少条原始消息 |

digest 消息的响应里另有 `aggregated_ids`，列出被汇总的原始消息 ID：

```json
{
  "message": {"id": "msg_digest...", "title": "3 条 backup 汇总", "level": "info", "digest_count": 3, "...": "..."},
  "status": "in_progress",
  "deliveries": [{"channel": "bark", "status": "pending"}],
  "aggregated_ids": ["msg_a...", "msg_b...", "msg_c..."]
}
```

每条 delivery 的 `rule` / `groups` 说明该渠道是被哪条规则、哪些组带进来的；fan-out 时 `groups` 可能有多个。升级前的老消息没有 `route` 字段。

不存在返回 404。

## GET /healthz

不需要鉴权。检查数据库连通性，并顺带暴露版本、运行时长与队列积压：

```json
{
  "status": "ok",
  "version": "v1.0.0",
  "uptime_seconds": 3600,
  "checks": {"database": "ok"},
  "queue": {"pending": 0, "sending": 1, "succeeded": 128, "failed": 2},
  "channels": ["bark", "dingtalk"]
}
```

| 情况 | 状态码 | `status` |
| --- | --- | --- |
| 数据库正常 | 200 | `ok` |
| 数据库不可用 | 503 | `unavailable`（`checks.database = unavailable`） |
| 数据库正常但队列统计失败 | 200 | `degraded`（`checks.queue = unavailable`） |

`queue` 里的积压不影响健康状态：消息还在重试，属于可恢复情况；持续上涨时按 [deploy.md](deploy.md#出问题先看什么) 排查。
