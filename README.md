# unipush

个人统一消息推送平台。任何脚本、服务、设备都只调用**一个** HTTP 接口，由平台按规则投递到 Bark、钉钉等渠道，带落盘队列、失败重试和完整投递记录。

## 解决什么

- 每个脚本各自维护推送配置和 token，加一个渠道要改 N 处
- 手机走 Bark、群消息走钉钉，没有统一调度
- 推送失败没人知道，重复推送又轰炸；排障没有记录可查

## 核心设计

1. 单一入口 `POST /v1/messages`，Bearer Token 鉴权，请求体归一化（`title` / `body` / `level` / `source` / `targets` / `dedupe_key` / `link` / `extra`）
2. 渠道适配器接口 `Sender{ Send(ctx, *Message) error }`，一期实现 Bark、钉钉，后续按需扩展
3. 路由规则：按 `source` + `level` + `targets` 决定投递到哪些渠道组
4. 投递队列：SQLite（WAL）落盘 outbox，先持久化再发送；指数退避重试，每渠道独立
5. 去重与聚合：同一 `dedupe_key` 在时间窗内合并，避免通知风暴
6. 可观测：`messages` / `deliveries` 两级记录，`GET /v1/messages/{id}` 查投递状态，`GET /healthz`

## 技术栈

- Go 单静态二进制，无运行时依赖
- SQLite（WAL）
- YAML 配置 + 环境变量注入密钥；token、webhook 地址不进仓库
- 本机运行，后续可选 Docker

## 目录约定

```
cmd/unipush/        # 入口
internal/config/    # 配置加载
internal/api/       # HTTP 接口
internal/channel/   # 渠道适配器（bark/、dingtalk/）
internal/store/     # SQLite 存取与迁移
internal/dispatch/  # 路由与投递 worker
docs/               # 设计说明与部署文档
```

## 状态

M1（消息入口 + Bark/钉钉 适配器 + 投递队列）开发中。规划与验收标准见 Multica 项目「统一消息推送平台」。

## 配置

复制 `.env.example` 为 `.env` 并填写：

```
UNIPUSH_AUTH_TOKEN=        # 调用方使用的 Bearer Token
BARK_DEVICE_KEY=           # Bark 设备 key
DINGTALK_WEBHOOK=          # 钉钉机器人 webhook 地址
DINGTALK_SECRET=           # 钉钉机器人加签 secret
```

真实值不要提交到仓库。
