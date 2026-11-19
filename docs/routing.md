# 路由：渠道组与投递规则（M2）

调用方只表达「这条消息是什么、从哪来、多严重」，平台按 `config.yaml` 里的 `routing` 段决定发到哪些渠道。加一个渠道、换一个群，只改配置，不改调用方代码，也不用重新发版。

## 概念

| 概念 | 说明 |
| --- | --- |
| 渠道组（group） | 一组渠道的命名集合，例如 `phone: [bark]`、`team: [dingtalk]`、`all: [bark, dingtalk]` |
| 规则（rule） | `sources` + `levels` 匹配后投递到 `groups`；按配置顺序求值，**第一条命中即生效** |
| 默认目标（default） | 没有命中任何规则时的兜底，值是渠道组列表 |

解析顺序：

1. 请求里带了 `targets` → 直接按它投递（`route.mode = explicit`），规则不参与；
2. 否则按顺序找第一条匹配的规则（`route.mode = rule`）；
3. 都没命中 → 投 `routing.default`（`route.mode = default`）；没有配置 default 则返回 `503 no_route`。

规则命中多个组时取渠道并集，同一渠道只投一次（fan-out 去重）。

## 配置

```yaml
routing:
  groups:
    phone: [bark]
    team: [dingtalk]
    all: [bark, dingtalk]

  default: [all]

  rules:
    - name: nas-alerts
      sources: ["nas-*"]
      levels: [warn, error]
      groups: [phone]

    - name: ci-build
      sources: [ci]
      levels: [info, warn]
      groups: [team]

    - name: error-everywhere
      levels: [error]
      groups: [all]
```

字段说明：

- `groups.<组名>`：渠道名列表，只接受本版本支持的渠道（当前 `bark`、`dingtalk`）。组名不能和渠道名重名，否则 `targets` 无法区分。
- `rules[].name`：必填且唯一，会写进消息的 `route.rule` 与每条 delivery 的 `rule`，用于排查。
- `rules[].sources`：来源匹配，支持 shell 通配（`path.Match` 语义，例如 `nas-*`、`*-monitor`、`*`）；精确匹配不区分大小写。留空表示不限来源。
- `rules[].levels`：`info` / `warn` / `error` 的精确列表；留空表示不限级别。
- `rules[].groups`：命中后投递的渠道组，至少一个。
- `rules` 必须至少有一个匹配条件（`sources` 或 `levels`），否则等于吞掉所有消息——这种配置会被拒绝，兜底请用 `default`。
- 整段 `routing` 可以不写：此时退化为 M1 行为，不带 `targets` 的消息投递到全部已启用渠道。

## 调用方怎么用

不传 `targets`，由规则决定：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title":"磁盘告警","body":"剩余空间 3%","level":"error","source":"nas-monitor"}'
```

显式指定目标（覆盖规则），`targets` 接受渠道组名，也接受渠道名：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer $UNIPUSH_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title":"磁盘告警","body":"剩余空间 3%","level":"error","source":"nas-monitor","targets":["team"]}'
```

## 启动校验

以下情况服务**直接启动失败**，错误信息带上 `config.yaml` 里的位置：

- 规则或默认目标引用了未定义的组：`routing.rules[0].groups[0]: 未定义的渠道组 "ghost"`
- 组里出现未知渠道：`routing.groups.phone.channels[1]: 未知渠道 "telegram"`
- 组为空、组名与渠道名冲突、规则重名、规则没有匹配条件、规则没有目标组、非法通配模式、未知级别

渠道凭据缺失（例如没配 `BARK_DEVICE_KEY`）不会让服务起不来，但会在启动日志里告警「渠道组 phone 里没有已启用的渠道」。命中这种组的消息会返回 `503 no_route` 并在错误里说明原因，而不是静默丢弃。

## 排查「为什么发到了这里」

`POST /v1/messages` 的响应和 `GET /v1/messages/{id}` 都带路由解析结果：

```json
{
  "message": {
    "id": "msg_3f6f...",
    "title": "磁盘告警",
    "level": "error",
    "source": "nas-monitor",
    "route": {
      "mode": "rule",
      "rule": "nas-alerts",
      "groups": ["phone"],
      "channels": ["bark"]
    }
  },
  "status": "succeeded",
  "deliveries": [
    {
      "id": "dlv_...",
      "channel": "bark",
      "status": "succeeded",
      "rule": "nas-alerts",
      "groups": ["phone"]
    }
  ]
}
```

- `route.mode`：`explicit`（调用方指定）/ `rule`（命中规则）/ `default`（默认目标）
- `route.groups`：命中的渠道组；`route.channels`：最终投递的渠道（已去重排序）
- 每条 delivery 的 `rule` / `groups` 指出该渠道是被哪条规则、哪些组带进来的；fan-out 时 `groups` 可能有多个

## 几个常见配置

全部消息都发手机：

```yaml
routing:
  groups: {phone: [bark]}
  default: [phone]
```

严重告警全渠道，日常信息只发群：

```yaml
routing:
  groups:
    phone: [bark]
    team: [dingtalk]
    all: [bark, dingtalk]
  default: [team]
  rules:
    - name: errors
      levels: [error]
      groups: [all]
```

按来源拆分，未识别的来源不发：

```yaml
routing:
  groups:
    phone: [bark]
    team: [dingtalk]
  rules:
    - name: nas
      sources: ["nas-*"]
      groups: [phone]
    - name: ci
      sources: [ci]
      groups: [team]
```

（没有 `default` 时，其他来源会收到 `503 no_route`，这是刻意保留的「不静默丢弃」。）
