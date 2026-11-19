# Web 控制台（M5）

控制台与 `unipush` 在同一个二进制里，模板和静态资源都已内嵌，不需要 Node、npm 或额外的前端服务。默认地址与 API 相同：`http://127.0.0.1:8080`。

## 第一次启动

数据库里还没有任何用户时，服务会创建初始管理员：

```bash
# 可选：显式指定初始账号；密码留空会自动生成
export UNIPUSH_ADMIN_USER=admin
export UNIPUSH_ADMIN_PASSWORD='<至少 8 位>'
go run ./cmd/unipush
```

- `UNIPUSH_ADMIN_PASSWORD` 留空时，服务生成随机密码并**只在启动日志里打印一次**，形如：
  `WARN 首次启动已创建 Web 控制台管理员，随机密码只打印这一次 ... username=admin password=...`
- 已有用户后，这两个环境变量不再生效，避免重启时意外重置密码。
- 登录后可以在「账户」页修改密码；管理员可以在「用户」页创建其他账号。

## 能做什么

| 页面 | 能力 |
| --- | --- |
| 概览 | 队列积压、已送达 / 失败数量、可用渠道与渠道组、最近消息 |
| 消息记录 | 按来源 / 级别 / 状态过滤，游标分页，查看每条消息的投递明细与路由原因 |
| 发送消息 | 等价于调用 `POST /v1/messages`，可显式选择渠道组 / 渠道 |
| API Token | 创建、查看、撤销 Token；明文只展示一次 |
| 用户（管理员） | 新增账号、重置密码、停用 / 启用账号 |
| 账户 | 修改自己的密码，查看角色与最近登录 |

### 多用户与权限

- 角色只有两种：`admin`（管理员）和 `member`（成员）。
- 所有登录用户都能查看消息记录、发送消息、管理自己的 Token。
- 只有管理员能进入「用户」页；不能停用当前登录账号，也不能停用最后一个启用的管理员。
- 停用用户会立即删除其全部登录会话；该用户名下所有 API Token 也同时失效（校验时会检查用户状态）。
- 重置密码会让该用户在其他设备上的会话失效；管理员重置自己的密码时保留当前会话。

### Token 怎么用

在「API Token」页填写用途名称（例如 `ci-build`）并生成。页面会显示一次完整 Token：

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer up_xxx" \
  -H "Content-Type: application/json" \
  -d '{"title":"部署完成","body":"v1.2.3 已上线","level":"warn","source":"ci"}'
```

说明：

- Token 以 `up_` 开头；数据库只保存 SHA-256 哈希，页面上也只显示前缀，明文无法再次取回。
- 撤销立即生效，之后再用会返回 `401`。
- `config.yaml` 里 `auth.tokens` 配置的静态 Token 仍然可用，适合保留给固定脚本；两者权限相同。
- Token 的「最近使用」最多每分钟更新一次，不会给每条消息都增加写事务。

## 安全说明

- 密码使用 bcrypt 哈希；登录会话只保存随机 Token 的 SHA-256，Cookie 为 `HttpOnly` + `SameSite=Lax`，HTTPS 下自动加 `Secure`。
- 所有写操作（发送消息、创建 / 撤销 Token、用户管理、改密码、退出）都校验会话绑定的 CSRF Token。
- 登录失败按「来源 IP + 用户名」限流：5 分钟内 5 次失败后暂时拒绝；成功登录会清零。
- 登录后只允许跳回站内路径，避免开放重定向。
- 响应带 CSP、`X-Frame-Options: DENY`、`nosniff` 等安全头；页面不使用内联脚本。
- 默认只监听 `127.0.0.1`。如果要放到公网或局域网，请放在反向代理后并启用 HTTPS，同时把 `X-Forwarded-Proto: https` 透传给服务，Cookie 才会带 `Secure`：

  ```nginx
  location / {
      proxy_pass http://127.0.0.1:8080;
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-Proto $scheme;
      proxy_set_header X-Real-IP $remote_addr;
  }
  ```

- 登录限流使用 TCP 对端地址，不信任 `X-Forwarded-For`，避免伪造头绕过或误伤。反向代理后所有请求会呈现为同一个 IP，此时限流只能起到全局兜底作用。

## 配置

```yaml
web:
  enabled: true          # false 时只保留 /v1 API 与 /healthz
  admin_user: ""         # 建议用 UNIPUSH_ADMIN_USER 注入
  admin_password: ""     # 建议用 UNIPUSH_ADMIN_PASSWORD 注入
  session_ttl: 168h      # 登录会话有效期
```

关闭控制台后，已有用户与 Token 仍保存在数据库里；重新打开即可继续使用。

## 常见问题

- **登录提示用户名或密码错误**：确认使用的是启动日志里的初始密码；若已修改过，管理员可在「用户」页为其他账号重置密码。
- **管理员密码也忘了**：停掉服务后执行 `./unipush -reset-password admin`，会生成随机新密码并打印；也可用
  `UNIPUSH_RESET_PASSWORD='新密码' ./unipush -reset-password admin` 指定。该操作会清掉该用户的全部登录会话。
- **刚创建的用户无法登录**：检查账号是否被停用；被停用的账号不能登录，其 Token 也会失效。
- **401 但 Token 看起来没错**：确认 Token 完整（以 `up_` 开头）、未被撤销、所属用户未停用；也可以在「API Token」页查看「最近使用」时间。
- **发消息 400 未知投递目标**：页面里的渠道 / 渠道组来自当前启用配置；`targets` 只能填这些名字。
- **`info` 消息一直显示「已聚合」**：`info` 会先进入聚合窗口（默认 1 分钟），生成 digest 后才投递；要立刻发送请选 `warn` / `error`。
