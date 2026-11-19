# 部署与运维（M4 + M5）

目标：一台机器上长期常驻，升级重启不丢队列，出问题能自己查。

## 选一种方式跑

| 方式 | 适用 | 入口 |
| --- | --- | --- |
| 本机二进制 + launchd / systemd | 已有 Go 环境，想直接用 | `go build` 后交给系统托管 |
| Docker Compose | 机器上有 Docker，想一条命令起 | `docker compose up -d --build` |
| 前台进程 | 临时调试 | `go run ./cmd/unipush` |

三种方式共用同一份 `config.yaml` 和 `.env`，数据库都落在 `./data/unipush.db`（Docker 里挂到 `/data`）。

### 1. 本机二进制

```bash
go build -o bin/unipush ./cmd/unipush
go build -o bin/push ./cmd/push

cp .env.example .env     # 填 token 与渠道凭据
./bin/unipush            # 前台运行，Ctrl+C 优雅退出
```

启动前检查：`./bin/unipush --version`；启动后检查：`curl -fsS http://127.0.0.1:8080/healthz`。

### 2. macOS launchd

`~/Library/LaunchAgents/com.lzf.unipush.plist`（路径按实际修改）：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.lzf.unipush</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/unipush/bin/unipush</string>
    <string>--config</string><string>/opt/unipush/config.yaml</string>
  </array>
  <key>WorkingDirectory</key><string>/opt/unipush</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/opt/unipush/logs/unipush.log</string>
  <key>StandardErrorPath</key><string>/opt/unipush/logs/unipush.log</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
```

```bash
launchctl load -w ~/Library/LaunchAgents/com.lzf.unipush.plist
launchctl list | grep unipush
launchctl unload -w ~/Library/LaunchAgents/com.lzf.unipush.plist
```

`WorkingDirectory` 必须是配置和 `.env` 所在目录，否则相对路径的数据库与密钥文件会找不到。日志建议配合 `newsyslog` 或定期轮转。

### 3. Linux systemd

`/etc/systemd/system/unipush.service`：

```ini
[Unit]
Description=unipush
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/unipush
EnvironmentFile=/opt/unipush/.env
ExecStart=/opt/unipush/bin/unipush --config /opt/unipush/config.yaml
Restart=on-failure
RestartSec=3
# 给优雅退出留足时间，把队列里正在发的消息发完。
KillSignal=SIGTERM
TimeoutStopSec=30
User=unipush
Group=unipush
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/unipush/data /opt/unipush/logs
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now unipush
systemctl status unipush
journalctl -u unipush -f          # 日志（text 格式时人读友好）
```

### 4. Docker Compose

需要 Docker Compose v2.24+（`env_file.required` 让 `.env` 变成可选；老版本请先 `cp .env.example .env` 并删掉 compose 里的 `required` 行）。

```bash
cp .env.example .env             # 填 UNIPUSH_AUTH_TOKEN 与渠道凭据
docker compose up -d --build
docker compose ps                # STATUS 应显示 healthy
curl -fsS http://127.0.0.1:8080/healthz
```

- 端口只映射到宿主机 `127.0.0.1:8080`，公网访问请自己加反向代理和 TLS。
- 配置：仓库的 `config.yaml` 以只读方式挂进容器；想单独维护就取消 `docker-compose.yml` 里 `config.local.yaml` 那行注释。
- 数据：`./data` 挂到容器 `/data`，与 `go run` 时的数据库路径一致。
- 日志：容器内是 JSON 格式，`docker compose logs -f unipush` 查看；compose 已限制单文件 10 MB × 3。
- 升级：`git pull && docker compose up -d --build`，队列在 `./data` 卷里，重启不丢。
- 容器以 uid/gid `10001` 运行。Linux 上如果 `./data` 是宿主用户创建的，需要放权：

  ```bash
  sudo chown -R 10001:10001 ./data
  ```

  macOS 的 Docker Desktop 一般不需要这一步。

如果机器上没装 Docker，跳过本节即可；本项目的定位是本机常驻，Docker 只是可选交付。

## 开启鉴权

`POST /v1/messages`、`GET /v1/messages`、`GET /v1/messages/{id}` 都需要 `Authorization: Bearer <token>`。

1. 在 `.env` 里设置 `UNIPUSH_AUTH_TOKEN=<足够长的随机串>`（`openssl rand -hex 24` 生成）。
2. 需要多个调用方用不同 token 时，在 `config.yaml` 里写列表：

   ```yaml
   auth:
     tokens:
       - ${UNIPUSH_AUTH_TOKEN}
       - ${UNIPUSH_AUTH_TOKEN_2}
   ```

3. 重启服务。没配 token 时服务会生成随机临时 token 并打印在启动日志里——只能本机调试，重启就变，生产必须配置固定 token。

也可以不把 token 写进配置文件：登录 Web 控制台 → 「API Token」→ 生成，页面只展示一次明文，数据库只存 SHA-256 哈希；撤销立即生效。静态配置 token 与 Web 创建的 token 权限相同，目前不做细粒度权限。

Web 控制台首次启动会创建管理员：优先使用 `UNIPUSH_ADMIN_USER` / `UNIPUSH_ADMIN_PASSWORD`，密码留空则随机生成并打印到启动日志。多用户、Token 与安全说明见 [web.md](web.md)。

## 配置项速查

配置文件默认 `config.yaml`，可用 `--config` 或 `UNIPUSH_CONFIG` 指定。YAML 里的 `${VAR}` 在启动时从环境变量展开；`.env`（当前工作目录）会被自动加载，已存在的环境变量优先。

### 环境变量

| 变量 | 作用 |
| --- | --- |
| `UNIPUSH_AUTH_TOKEN` | 调用方 Bearer Token，至少一个 |
| `UNIPUSH_ADMIN_USER` | 首次启动的 Web 管理员用户名（默认 `admin`） |
| `UNIPUSH_ADMIN_PASSWORD` | 首次启动的管理员密码；留空随机生成并打印一次 |
| `UNIPUSH_ADDR` | 覆盖 `server.addr`（Docker 用 `0.0.0.0:8080`） |
| `UNIPUSH_DB_PATH` | 覆盖 `database.path`（Docker 用 `/data/unipush.db`） |
| `UNIPUSH_LOG_FORMAT` | 覆盖 `log.format`：`text` / `json` |
| `UNIPUSH_LOG_LEVEL` | 覆盖 `log.level`：`debug` / `info` / `warn` / `error` |
| `UNIPUSH_CONFIG` | 配置文件路径，默认 `config.yaml` |
| `BARK_DEVICE_KEY` / `DINGTALK_WEBHOOK` / `DINGTALK_SECRET` | Bark / 钉钉凭据 |
| `FEISHU_WEBHOOK` / `FEISHU_SECRET` | 飞书机器人 |
| `WECOM_WEBHOOK` | 企业微信群机器人 |
| `SMTP_USERNAME` / `SMTP_PASSWORD` / `SMTP_FROM` | 邮件渠道 |
| `WEBHOOK_URL` | 通用 webhook 目标 |

### 配置段

| 段 | 关键项（默认值） | 说明 |
| --- | --- | --- |
| `server` | `addr`（`127.0.0.1:8080`）、`read_timeout` / `write_timeout` / `idle_timeout`、`shutdown_timeout`（10s）、`max_body_bytes`（1 MiB） | 监听地址与 HTTP 超时 |
| `auth` | `tokens` | Bearer Token 列表 |
| `database` | `path`（`./data/unipush.db`） | SQLite 路径，自动建目录、WAL、自动迁移 |
| `web` | `enabled`（true）、`session_ttl`（168h） | Web 控制台开关与会话有效期；`admin_*` 只在首次启动生效 |
| `dispatch` | `workers`（4）、`poll_interval`（1s）、`lease`（60s）、`max_attempts`（8）、`base_backoff`（2s）、`max_backoff`（5m）、`request_timeout`（15s） | 投递 worker 与重试 |
| `channels.*` | `enabled` + 各自的 webhook/凭据 | 缺少凭据的渠道会自动禁用并在启动日志告警 |
| `routing` | `groups` / `default` / `rules` | 渠道组与投递规则，详见 [routing.md](routing.md) |
| `dedupe` | `enabled`（true）、`window`（5m） | 同一 `dedupe_key` 窗口内只投一次 |
| `aggregation` | `enabled`（true）、`window`（1m）、`max_items`（50） | `info` 消息聚合 |
| `log` | `format`（text）、`level`（info） | 结构化日志；容器里建议 `json` |

配置自检：路由引用不存在的组/渠道、日志格式写错等，服务会在开始监听**之前**报错退出，不会带着坏配置跑。

## 用 CLI 发消息（脚本 / cron）

```bash
./bin/push send \
  --title "备份完成" \
  --body "3 个文件，共 1.2 GB" \
  --level info --source nas-monitor
```

- `--url` / `--token` 不传时取 `UNIPUSH_URL` / `UNIPUSH_AUTH_TOKEN`，并自动加载当前目录 `.env`，所以常用写法就是上面那样。
- `--body -` 从标准输入读正文；`--quiet` 只打印消息 ID，方便脚本取值；`--json` 打印原始响应。
- 退出码：`0` 成功、`1` 请求或服务端错误、`2` 用法/配置错误，cron 里可以直接用。

cron 示例（每 5 分钟检查一次磁盘，异常才推）：

```cron
*/5 * * * * cd /opt/unipush && ./bin/push send --title "NAS 巡检" --source nas-monitor --level info --body "$(df -h /data | tail -1)" >> /var/log/unipush-cron.log 2>&1
```

失败通知示例：命令失败才发一条 error，并带上去重键，避免同一故障重复轰炸：

```bash
cd /opt/unipush
if ! ./scripts/check.sh; then
  ./bin/push send --title "check.sh 失败" --source cron-check --level error \
    --dedupe-key "check-failed" --body "详见 $(hostname) 上的 /var/log/check.log"
fi
```

## 只读消息列表

没有 Web 控制台（一期非目标），查询用接口：

```bash
# 最近 20 条
curl -sS "http://127.0.0.1:8080/v1/messages" -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"

# 只看失败的，按来源过滤
curl -sS "http://127.0.0.1:8080/v1/messages?status=failed&source=nas-monitor&limit=50" \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"

# 翻页：把上一页的 next_cursor 原样传回来
curl -sS "http://127.0.0.1:8080/v1/messages?limit=20&cursor=<next_cursor>" \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN"
```

参数与响应字段见 [api.md](api.md#get-v1messages)。列表只读，不会触发重发。

## 备份与恢复 SQLite

数据文件：`data/unipush.db` + 同目录的 `unipush.db-wal`、`unipush.db-shm`（WAL 模式）。

Web 登录用户、会话与 API Token 都在同一个库里（密码和 Token 只存哈希），备份数据库就同时备份了控制台账号。

> 不要在服务运行时只 `cp` 主库文件：最近的写入还在 WAL 里，拷出来可能缺数据。

**在线备份（推荐）** — 用 SQLite 的 backup API，运行中也能拿到一致快照：

```bash
mkdir -p backups
sqlite3 ./data/unipush.db ".backup 'backups/unipush-$(date +%F-%H%M).db'"
sqlite3 "backups/unipush-$(date +%F-%H%M).db" "PRAGMA integrity_check; SELECT COUNT(*) FROM messages;"
```

**停机备份** — 更直观，适合和升级一起做：

```bash
docker compose stop unipush        # 或 launchctl/systemctl stop
cp -a ./data/unipush.db* backups/
docker compose start unipush
```

**恢复**：

```bash
# 1. 停服务（保证没有进程持有数据库）
docker compose stop unipush        # 或 sudo systemctl stop unipush
# 2. 把坏库和残留 WAL/SHM 挪走，别混在一起
mkdir -p data.broken && mv ./data/unipush.db* data.broken/ 2>/dev/null || true
# 3. 放回备份，并确保目录属主可写
cp backups/unipush-2026-01-01-1200.db ./data/unipush.db
sudo chown 10001:10001 ./data/unipush.db   # Docker 部署时需要
# 4. 起服务，确认健康检查与历史消息可查
docker compose start unipush && curl -fsS http://127.0.0.1:8080/healthz
```

恢复后 `unipush.db-wal` / `-shm` 会由 SQLite 重新创建，不要从备份里一起拷回来。

## 升级

**二进制部署**：

```bash
cd /opt/unipush
git pull
go build -o bin/unipush.new ./cmd/unipush && mv bin/unipush.new bin/unipush
go build -o bin/push.new ./cmd/push && mv bin/push.new bin/push
# 重启（launchctl kickstart -k 或 systemctl restart）
```

**Docker 部署**：

```bash
git pull
docker compose up -d --build
docker compose ps        # 等 STATUS 变成 healthy
```

- 数据库迁移在启动时自动执行，只向前、不自动回滚；升级前先备份。
- 重启时处于 `sending` 的投递会重新入队，已 `succeeded` 的渠道不会重发（见下节）。
- 想回滚：换回旧二进制 + 恢复升级前的备份（新库结构可能不被旧版本识别）。

## 出问题先看什么

按这个顺序排查，基本能定位到层：

1. **进程活着吗**：`curl -fsS http://127.0.0.1:8080/healthz`。
   - 返回 `503` 且 `checks.database = unavailable`：数据库路径不可写/文件损坏，先看启动日志和目录权限。
   - `status = degraded`：数据库正常但队列统计查不了，通常是瞬时锁，重试即可。
   - 连不上：进程没起来，看启动日志（端口占用、配置校验失败最常见）。
2. **是鉴权问题吗**：接口返回 `401` = token 缺失/不对/已撤销。静态 token 改完 `.env` 要重启；Web 控制台创建的 token 可在「API Token」页看状态与最近使用时间，所属用户被停用时也会 401。
3. **是路由问题吗**：`503 no_route` / `400 invalid_request` 的 `error.message` 会写明是哪条规则、哪个组没有可用渠道。看 `config.yaml` 的 `routing`，以及启动日志里「渠道已禁用」的告警（凭据没配）。
4. **消息卡在哪**：`GET /v1/messages/{id}` 看每条 delivery 的 `status`、`attempts`、`last_error`、`next_attempt_at`。
   - `pending` + `next_attempt_at` 在未来：正在退避重试，等即可。
   - `failed`：重试耗尽或不可重试错误，按 `last_error` 处理。
5. **按渠道看具体错误**：
   - 钉钉 `310000 keywords not in content`：机器人开了「自定义关键词」，在 `channels.dingtalk.keyword` 填上关键词（正文会自动拼成 `关键词: 内容`）。
   - 钉钉 `sign not match` / 时间戳错误：检查 `DINGTALK_SECRET` 和机器时钟。
   - Bark 401/404：设备 key 过期或错误。
   - 飞书 `sign match fail`：`FEISHU_SECRET` 与机器人加签密钥不一致。
   - 邮件超时：`channels.email.host/port/tls` 与 SMTP 服务不匹配（465 用 `ssl`，587 用 `starttls`）。
   - 通用 webhook 非 2xx：`last_error` 里有响应码，必要时在目标端看日志。
6. **队列积压**：`/healthz` 的 `queue.pending` 持续上涨。看渠道是否整体不可达，临时把 `dispatch.request_timeout` 调小、或关掉出问题的渠道。
7. **日志**：`log.format: json` 时按字段过滤，例如 `docker compose logs unipush | grep '"level":"ERROR"'`、`jq 'select(.message_id=="msg_...")'`。访问日志包含 `method` / `path` / `status` / `duration_ms` / `remote`。

### 常用排查命令

```bash
# 最近失败的消息
curl -sS "http://127.0.0.1:8080/v1/messages?status=failed&limit=20" \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" | jq '.messages[] | {id, source, title, deliveries}'

# 队列里还在重试的
curl -sS "http://127.0.0.1:8080/v1/messages?status=in_progress" \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" | jq '.messages[] | {id, deliveries}'

# 直接看库（服务运行时只读查询是安全的）
sqlite3 ./data/unipush.db "SELECT status, COUNT(*) FROM deliveries GROUP BY status;"
```
