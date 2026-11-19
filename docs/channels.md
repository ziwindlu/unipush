# 渠道配置与实测命令（M3）

M3 在 Bark / 钉钉之外补齐了 4 个渠道：**飞书**、**企业微信**、**SMTP 邮件**、**通用 HTTP webhook**。
所有渠道都实现同一个 `Sender{ Send(ctx, *Message) error }` 契约，配置写在 `config.yaml` 的 `channels` 段，密钥只从环境变量 / `.env` 注入。

每个渠道的凭据没有配好时会在启动时自动禁用并打印告警，不会让服务起不来；路由里引用的渠道全都不可用时，消息入口返回 `503`。

## 渠道一览

| 渠道名 | 配置段 | 必填 | 说明 |
| --- | --- | --- | --- |
| `feishu` | `channels.feishu` | `webhook` | 自定义机器人，支持加签与卡片 |
| `wecom` | `channels.wecom` | `webhook` | 群机器人，markdown / text |
| `email` | `channels.email` | `host`、`from`、`to` | multipart/alternative（纯文本 + HTML） |
| `webhook` | `channels.webhook` | `url` | 任意 HTTP 接口，请求体可模板化 |

`config.yaml` 的样例里 `all` 组已经包含全部渠道，用 `targets: ["all"]` 即可一次打到所有已启用的渠道。

---

## 飞书（feishu）

```yaml
channels:
  feishu:
    enabled: true
    webhook: ${FEISHU_WEBHOOK}   # https://open.feishu.cn/open-apis/bot/v2/hook/xxxx
    secret: ${FEISHU_SECRET}     # 机器人「加签」密钥，未开启加签留空
    msg_type: text               # text（默认）| card
```

- 加签：`sign = base64(HMAC-SHA256(key = timestamp + "\n" + secret, msg = ""))`，`timestamp`（Unix 秒）与 `sign` 作为请求体顶层字段发送，算法与飞书官方示例一致。
- `msg_type: text` 发送纯文本（标题 / 正文 / 链接逐行拼接）；`card` 发送带标题与配色的卡片，`info` 蓝、`warn` 橙、`error` 红。
- 响应 `code != 0` 视为失败：限流类错误码（如 `9499`）与 HTTP 429/5xx 会重试，参数/权限类错误直接标记失败。

实测（先 `cp .env.example .env` 并填好 `FEISHU_WEBHOOK`）：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title": "飞书联调", "body": "来自 unipush 的测试消息", "level": "warn", "targets": ["feishu"]}'
```

## 企业微信（wecom）

```yaml
channels:
  wecom:
    enabled: true
    webhook: ${WECOM_WEBHOOK}    # https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxx
    msg_type: markdown           # markdown（默认）| text
```

- `markdown` 正文形如 `**标题**\n正文\n[详情](链接)`；群机器人 markdown 内容上限 **4096 字节**，超长会按 UTF-8 字符边界截断并补 `…`。
- 机器人限流约 **20 条/分钟**；`errcode` 为 `45009`（频率限制）、`45047` 等会按指数退避重试，参数/鉴权类错误直接失败。
- `text` 形态只拼接标题与正文，不带链接。

实测：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title": "企微联调", "body": "**粗体** 与 [链接](https://example.com)", "level": "info", "targets": ["wecom"]}'
```

> 注意：`level: info` 会进入聚合窗口，默认 1 分钟后才发 digest。想立刻看到效果，先用 `level: warn`，或把 `aggregation.enabled` 设为 `false`。

## SMTP 邮件（email）

```yaml
channels:
  email:
    enabled: true
    host: smtp.qq.com
    port: 587
    username: ${SMTP_USERNAME}
    password: ${SMTP_PASSWORD}     # QQ 邮箱用授权码，不是登录密码
    from: ${SMTP_FROM}             # 发件人地址，例如 xxx@qq.com
    to: ["me@example.com"]         # 收件人列表，至少一个
    tls: starttls                  # starttls（默认，587）| ssl（465）| none（内网中继）
    subject_prefix: "[unipush]"
```

- 邮件是 `multipart/alternative`：纯文本 + HTML 两个部分，HTML 里正文换行转 `<br>`、链接渲染成 `<a href>`，所有字段做 HTML 转义。
- 主题用 RFC 2047 编码（中文主题不会乱码），`subject = 前缀 + 空格 + 标题`。
- 收件人不存在（SMTP `5xx`）直接标记失败；连接失败、超时、`4xx` 会重试。

实测：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title": "邮件联调", "body": "正文支持换行\n第二行", "level": "warn", "targets": ["email"], "link": "https://example.com"}'
```

## 通用 HTTP webhook（webhook）

```yaml
channels:
  webhook:
    enabled: true
    url: ${WEBHOOK_URL}
    method: POST
    headers:
      X-Token: ${MY_HOOK_TOKEN}
    content_type: "application/json; charset=utf-8"
    body_template: |
      {"text": "[{{.Level}}] {{.Title}}\n{{.Body}}", "source": "{{.Source}}", "link": "{{.Link}}"}
```

- `body_template` 是 Go `text/template`，模板数据内嵌消息本身：`{{.Title}}` `{{.Body}}` `{{.Level}}` `{{.Source}}` `{{.Link}}` `{{.DedupeKey}}` `{{.ID}}`，另有 `{{.JSON}}`（整条消息的 JSON）与 `{{.Extra.xxx}}`。
- 模板留空时直接把消息的 JSON 发过去，适合直接对接自建脚本 / 服务。
- 模板写错会在**启动时**报错，不会等到投递才发现。
- 2xx 成功；4xx（除 408/429）不可重试；408/429/5xx/网络错误按退避重试。

实测：

```bash
# 先用一个临时服务看请求体
python3 -m http.server 9000 &   # 或者用你自己的接收端

# config.yaml: channels.webhook.url: http://127.0.0.1:9000/hook
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title": "webhook 联调", "body": "console.log 验证请求体", "level": "warn", "targets": ["webhook"]}'
```

---

## 去重（dedupe）

```yaml
dedupe:
  enabled: true
  window: 5m
```

带 `dedupe_key` 的消息在 `window` 内只真正投递一次，后续重复只给原消息累加 `merged_count`。窗口从**第一条**消息的创建时间开始算，不会因为后续重复延长。

```bash
for i in $(seq 1 10); do
  curl -sS -X POST http://127.0.0.1:8080/v1/messages \
    -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"title": "磁盘告警", "body": "剩余 3%", "level": "error", "source": "nas-monitor", "dedupe_key": "nas-disk-42"}'
done
```

预期：第一次返回 `"status": "queued"`，后 9 次返回 `"status": "deduped"` 与递增的 `"merged": 1..9`；
`GET /v1/messages/{id}` 里的 `message.merged_count` 为 `9`，`deliveries` 只有一条（每个目标渠道一条）。

## 聚合（aggregation）

```yaml
aggregation:
  enabled: true
  window: 1m
  max_items: 50
```

- 只有 `info` 级消息聚合；`warn` / `error` 一律立即投递。
- 聚合桶按「来源 + targets」划分：同来源且投递目标相同的 info 消息才会合成一条 digest，避免把只发手机的和发群的消息拼在一起后重复投递。
- 第一条消息进桶后等 `window`，或攒够 `max_items` 立即发送；进程优雅退出时会把桶里剩下的消息发掉。
- digest 的标题形如「3 条 backup 汇总」，正文逐条列出时间与内容摘要；`GET /v1/messages/{digest_id}` 的 `aggregated_ids` 列出被汇总的原始消息 ID，原始消息的 `message.aggregated_into` 指向 digest。

```bash
for i in 1 2 3; do
  curl -sS -X POST http://127.0.0.1:8080/v1/messages \
    -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"title\": \"备份完成 $i\", \"body\": \"耗时 32s\", \"level\": \"info\", \"source\": \"backup\"}"
done
```

预期：三次都返回 `"status": "aggregated"`、`deliveries` 为空；约 1 分钟后一条 digest 投递到默认渠道。
把最后一条改成 `"level": "error"`，它会立即投递，不参与聚合。
