# unipush

个人统一消息推送平台。任何脚本、服务、设备都只调用**一个** HTTP 接口，由平台按规则投递到 Bark、钉钉等渠道，带落盘队列、失败重试和完整投递记录。

## 解决什么

- 每个脚本各自维护推送配置和 token，加一个渠道要改 N 处
- 手机走 Bark、群消息走钉钉，没有统一调度
- 推送失败没人知道，重复推送又轰炸；排障没有记录可查

## 状态

M2 已完成：在 M1（单一入口 + Bearer 鉴权、Bark / 钉钉适配器、SQLite 落盘队列、指数退避重试、投递状态查询、优雅退出）之上加了**渠道组与投递规则**——按来源/级别决定发到哪里，调用方不改代码；显式 `targets` 可覆盖规则；配置写错（未知组 / 未知渠道）启动即失败；路由结果随消息与投递记录落库，可查「为什么发到了这里」。更多渠道与去重聚合（M3）、Docker/CLI/部署文档（M4）在后续任务中实现。

## 快速开始

需要 Go 1.23+。

```bash
cp .env.example .env
# 编辑 .env：至少填 UNIPUSH_AUTH_TOKEN；再填 BARK_DEVICE_KEY、DINGTALK_WEBHOOK、DINGTALK_SECRET
go run ./cmd/unipush
```

服务默认监听 `127.0.0.1:8080`，数据落在 `./data/unipush.db`。配置见 `config.yaml`，启动时会自动加载当前目录的 `.env`（已存在的环境变量优先）。

发一条消息：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "部署完成",
    "body": "v1.2.3 已上线",
    "level": "warn",
    "source": "ci",
    "link": "https://example.com/run/1"
  }'
```

返回 `202 Accepted`。`config.yaml` 里 `ci` + `warn` 命中规则 `ci-build` → 组 `team`，所以只排队到钉钉；响应里的 `route` 说明这次为什么发到这里：

```json
{
  "id": "msg_...",
  "status": "queued",
  "created_at": "2026-01-01T00:00:00Z",
  "route": {"mode": "rule", "rule": "ci-build", "groups": ["team"], "channels": ["dingtalk"]},
  "deliveries": [
    {"id": "dlv_...", "channel": "dingtalk", "status": "pending", "groups": ["team"]}
  ]
}
```

想同时发到手机就在请求里加 `"targets": ["all"]`（显式 targets 覆盖规则）。路由规则与配置写法见 [docs/routing.md](docs/routing.md)。

查询投递结果：

```bash
curl -sS http://127.0.0.1:8080/v1/messages/msg_... -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"
```

`status` 聚合为 `queued` / `in_progress` / `succeeded` / `partial` / `failed`；每条 delivery 带 `attempts`、`last_error`、`next_attempt_at`。

## 接口

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/v1/messages` | Bearer | 接收消息，归一化后落库并排队 |
| GET | `/v1/messages/{id}` | Bearer | 查询消息与每个渠道的投递状态 |
| GET | `/healthz` | 无 | 健康检查（含数据库连通性） |

请求体字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `title` | 与 `body` 至少一个 | 标题，≤200 字符 |
| `body` | 与 `title` 至少一个 | 正文，≤8000 字符 |
| `level` | 否 | `info`（默认）/ `warn` / `error` |
| `source` | 否 | 来源标识，路由规则按它匹配（例如 `ci`、`nas-monitor`） |
| `targets` | 否 | 显式指定投递目标：渠道组名（如 `phone`、`team`）或渠道名；不传则按路由规则决定 |
| `dedupe_key` | 否 | 去重键，M1 先记录，M3 实现合并 |
| `link` | 否 | http(s) 链接，Bark 可点击打开 |
| `extra` | 否 | 任意附加字段，原样保存 |

错误统一返回：

```json
{"error": {"code": "invalid_request", "message": "level 必须是 info / warn / error 之一", "field": "level"}}
```

## 投递保证

- **先落库再发送**：消息与各渠道投递记录在同一事务里写入，之后才唤醒 worker；进程在任何时刻崩溃都不会丢消息。
- **每渠道独立重试**：失败按 `base_backoff * 2^(attempts-1)` 退避，封顶 `max_backoff`，达到 `max_attempts` 后标记 `failed`。鉴权失败、device key 错误等不可重试错误立即失败，不浪费重试。
- **重启不重不发**：启动时把上次处于 `sending` 的投递重新入队；已 `succeeded` 的渠道不会再次发送。
- **渠道隔离**：单个渠道失败或超时不影响其他渠道。
- **路由可解释**：每次投递都记录命中的规则与渠道组（`route` / `delivery.rule` / `delivery.groups`），排障不用猜；路由配置错误在启动时暴露，不会静默丢消息。

## 配置

`config.yaml` 里的 `${VAR}` 会在启动时从环境变量展开，密钥只走环境变量 / `.env`，仓库里只有 `.env.example` 占位：

```bash
UNIPUSH_AUTH_TOKEN=        # 调用方 Bearer Token，至少一个；不配置会生成随机临时 token 并打印到日志
BARK_DEVICE_KEY=           # Bark 设备 key
DINGTALK_WEBHOOK=          # 钉钉机器人 webhook（含 access_token）
DINGTALK_SECRET=           # 钉钉加签 secret（未开启加签可留空）
```

常用配置项见 `config.yaml` 注释；数据库路径、worker 数量、重试次数、退避上下限、请求超时都可调。

`routing` 段定义渠道组与投递规则，是「消息发到哪」的唯一来源，完整说明见 [docs/routing.md](docs/routing.md)。

## 测试

```bash
go test ./...
go test -race ./...
```

- `internal/channel/bark` / `internal/channel/dingtalk`：基于 `httptest` 验证请求体、Bark 级别映射、钉钉 HMAC-SHA256 加签、错误分类（可重试 / 不可重试）
- `internal/store`：落库、到期认领、租约、重启恢复、状态回写
- `internal/routing`：按来源/级别命中规则、默认目标、显式 targets 覆盖、fan-out 去重、禁用渠道过滤，以及未知组 / 未知渠道 / 非法级别的启动校验
- `internal/dispatch`：多渠道路由、按规则选渠道、显式覆盖、重试后成功、重试耗尽、永久失败、断网恢复、重启不重复发送
- `internal/api`：401 / 400 / 413 / 503、字段归一化、查询聚合状态，以及两条端到端用例（同时打到 Bark + 钉钉；按来源路由 / 显式覆盖 / 查询路由结果）

## 目录

```
cmd/unipush/        # 入口与优雅退出
internal/api/       # HTTP 接口与鉴权
internal/channel/   # 渠道适配器契约（bark/、dingtalk/）
internal/config/    # YAML + .env 加载与校验
internal/dispatch/  # 投递 worker、重试与退避
internal/model/     # 消息、投递记录与路由结果模型
internal/routing/   # 渠道组与投递规则的编译、校验与解析
internal/store/     # SQLite（WAL）存取与迁移
docs/               # 接口与路由说明
```

## 里程碑

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| M1 | 消息入口、Bark/钉钉适配器、SQLite outbox、重试 | ✅ 已完成 |
| M2 | 渠道组与按来源/级别的路由规则 | ✅ 已完成 |
| M3 | 飞书/企微/邮件/Webhook、去重与聚合 | 待开始 |
| M4 | Docker、CLI、只读消息列表、部署文档 | 待开始 |
