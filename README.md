# iCloud 隐私邮箱归档 API v2

本服务把 iCloud 隐私邮箱的新邮件归档到本地，并且只向外提供两种取件能力：

1. `GET /api/v1/otp`：按邮箱返回持续累积的验证码历史。
2. 标准只读 IMAPS：按邮箱读取完整 MIME；正文已淘汰或不可用时返回保留原标题的占位邮件。

`POST /oauth2/v2.0/token` 只为 IMAPS 的 XOAUTH2 登录签发一小时访问令牌，不构成第三种取件方式。服务签发的 API Key、IMAP 密码、client ID、refresh token 和 access token 都是本服务凭据，不是 Microsoft 凭据。

> [!WARNING]
> 本项目是非官方自托管工具，依赖 iCloud IMAP 和可能变化的 Apple Web 接口。上线前请使用可承受风险的账号验证登录、隐私邮箱目录同步和真实投递，并把 PostgreSQL、keys 卷与 mail archive 卷纳入同一个备份恢复点。本服务适合作为取码和邮件归档入口，不应成为重要邮件的唯一副本。

## 核心能力

- 管理多个 iCloud 主号及其隐私邮箱，继续支持目录同步和自动创建。
- 升级后归档同步游标之后的全部新 UID，包括上游已读和未读邮件。
- 新建隐私邮箱拥有独立、版本化的 API Key、IMAP 密码、client ID 和 refresh token；迁移前已经领取的旧 alias 保留 legacy API Key 和直达链接。
- 通过派生取码 URL 或 Bearer API Key 重复读取最近 100 条验证码。
- 通过密码或 XOAUTH2 登录只读 IMAPS，读取完整 MIME、稳定本地 UID 和归档占位邮件。
- 原始 MIME 按 SHA-256 去重保存到独立卷，容量超限时只淘汰最早正文，永久保留邮件元数据。
- 管理界面和首选管理 API 使用首次启动生成的随机路径；固定 `/admin/api/v1` 仅作为旧客户端兼容 API 入口。

```text
iCloud 主号 INBOX
  └─ 增量同步与 MIME 去重
      ├─ 隐私邮箱 A ─ OTP API / 只读 IMAPS
      ├─ 隐私邮箱 B ─ OTP API / 只读 IMAPS
      └─ 隐私邮箱 C ─ OTP API / 只读 IMAPS
```

## 快速启动

要求 Docker Engine、Docker Compose v2。使用 iCloud 模式时，还需要可用的 iCloud 主号 App 专用密码；仅使用自定义邮箱模式时不需要。

```bash
docker compose up -d --build --wait
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
```

HTTP 默认只发布在 `127.0.0.1:8080`，IMAPS 默认只发布在 `127.0.0.1:1993`。首次启动会在 keys 卷中生成管理员密码、随机管理路径、外部登记接口的 OAuth token、主密钥，以及本地持久化 IMAPS 自签证书。

```bash
docker compose exec -T icloud-api cat /app/keys/admin-password
docker compose exec -T icloud-api cat /app/keys/admin-path
docker compose exec -T icloud-api cat /app/keys/oauth-token
docker compose exec -T icloud-api cat /app/keys/public-imap-cert.pem
```

`admin-path` 的值形如 `/<32位小写十六进制>/admin/`。管理界面、静态资源和前端路由跟随这个随机前缀，管理 API 的首选入口是去掉该值结尾 `/` 后再拼接 `/api/v1`。OpenAPI 的 `{admin_path}` 变量则使用去掉首尾 `/` 的值。为兼容升级前客户端，同一套 JSON 管理 API 也保留在固定 `/admin/api/v1`；固定 `/admin` 不提供管理界面。登录成功会为两个 API 路径签发同一会话的受限 Cookie，两个入口都执行相同的登录限流、会话认证和 CSRF 校验。管理响应默认使用 `Cache-Control: no-store, private`，返回明文凭证的处理器会覆盖为 `no-store`。随机路径只能降低未授权扫描噪声，不应被当作访问控制边界。

使用 `admin` 和首次生成的密码登录随机管理路径，添加 iCloud 主号后同步或手动登记隐私邮箱。管理端支持创建邮箱分组，并在“全部隐私邮箱”或主号详情中把单个、勾选的隐私邮箱移动到所选分组；删除分组不会删除邮箱，只会将其恢复为未分组。公开接口说明位于 <http://127.0.0.1:8080/docs/>，机器可读契约见 [`docs/openapi.yaml`](docs/openapi.yaml)。

每个主号可配置上游隐式 TLS IMAP 主机、端口和登录用户名，默认是 `imap.mail.me.com:993`。已有隐私邮箱后仍可修改这三项，但这代表切换邮箱来源：服务会清除该主号旧来源的同步游标、v1 快照、消费与 `Seen` 状态、v2 归档和 OTP 历史，轮换公开 IMAPS 的 `UIDVALIDITY`，再从新来源建立不回填历史的基线。单纯修改 IMAP 密码（iCloud 模式使用 App 专用密码）或重新启用主号只重置同步状态，不删除已有邮件。已有隐私邮箱后主号邮箱地址仍不可修改。

添加主号时也可以选择“自定义邮箱”。自定义模式单独保存邮箱后缀（例如 `example.com`），同一后缀只能配置一个主号；IMAP 密码使用 `imap_password` 提交并按原值加密保存。它不会调用 Apple，也不会改变 iCloud 隐私邮箱原有的每小时自动创建规则。在主号详情中输入生成数量即可批量生成随机地址，格式为 8–12 位小写英文字母和数字加 `@后缀`，同一批次和全局地址表都会阻止重复，地址也不能与主号的 IMAP 登录身份相同。单次最多生成 1000 个，可多次分批生成，`custom` 主号的累计数量不设上限。自定义地址的删除只清理本地记录，不会请求 Apple。

## 管理端凭证与复制格式

新建或已领取 v2 凭证的隐私邮箱会显示完整的 API Key、IMAP 密码、client ID 和 refresh token。迁移前已经领取的 legacy alias 只显示已有 Key 前缀和旧直达链接，不会伪造不存在的 v2 凭证。管理端支持单条、勾选批量和全部复制；输出一行一个邮箱、无表头。

取码链接严格使用五个横线：

```text
alias@example.com-----https://HOST/api/v1/otp?token=DERIVED_TOKEN
```

IMAP/OAuth 严格使用四个横线：

```text
alias@example.com----IMAP_PASSWORD----CLIENT_ID----REFRESH_TOKEN
```

管理 API 的凭证响应设置 `Cache-Control: no-store`。legacy alias 使用兼容接口只轮换 API Key 和由其派生的旧直达链接，保留消费记录、邮件快照、IMAP `Seen` 状态及 credential mode。v2 alias 可显式轮换整套凭证，使旧 API Key、派生取码 URL、IMAP 密码、refresh token 和所有已签发 access token 同时失效。迁移本身不会轮换已领取的 legacy Key。

## OTP API

可以使用完整 API Key 的 Bearer 鉴权：

```bash
curl -H 'Authorization: Bearer API_KEY' \
  'https://HOST/api/v1/otp'
```

也可以直接访问管理端复制的派生取码 URL：

```bash
curl 'https://HOST/api/v1/otp?token=DERIVED_TOKEN'
```

成功响应是裸 JSON 数组，最新优先，每个邮箱最多保留 100 个非空 OTP 值：

```json
[{"otp":"123456","time":"2026-08-11T12:00:00+08:00"}]
```

没有验证码时返回 `200 []`。重复请求不会消费验证码、不会改变本地状态，也不会给上游邮件设置已读标志。v2 的 OTP token 与 recent-mail 消费 token 按用途分别签名，不能通过改写 URL 路径互换；原 v1 直达 token 只继续用于 legacy alias 的 `/api/v1/mail/recent`。OTP 从主题、纯文本和 HTML 可读文本中提取，只接受未与字母或数字相邻的 4–8 位 ASCII 数字；每封邮件最多保存一个候选。

## 旧接口兼容

PR2 保留原有 v1 外部接口，已有调用方无需改 URL 或鉴权方式：

| 接口 | 鉴权 | 兼容语义 |
| --- | --- | --- |
| `GET /api/v1/mail/latest` | `Authorization: Bearer <API Key>` | 可重复读取当前最新完整快照，不消费邮件、不改变已读状态 |
| `GET /api/v1/mail/recent` | `?api_key=<API Key、legacy 旧直达 token 或 v2 recent-mail token>` | 返回最近一小时内当前快照；成功后记录消费并异步写入 IMAP `Seen` |
| `POST /api/v1/aliases` | `Authorization: Bearer <OAuth Token>` | 使用 query、表单或二者组合传递既有 `add_hide_my_eamil` 和 `icloud` 字段；合并后每个字段必须恰好出现一次 |

旧 alias 的 API Key、旧直达链接和消费状态继续有效。迁移前已经领取的 alias 保持 `legacy` 模式；新建 alias，以及迁移前存在 pending 一次性 Key 但尚未领取的 alias，才会补齐 v2 凭证。`latest_messages`、`consumed_messages`、`imap_seen_tasks` 和 `pending_alias_api_keys` 数据会保留并继续参与兼容流程。

外部登记接口仍读取 `ICLOUD_API_OAUTH_TOKEN`（或等价的部署密钥注入），升级过程不应删除已配置的 OAuth Token。成功响应继续使用 `api_key` 和 `mail_api_direct_link` 字段；管理 API 的 alias DTO 继续提供 `direct_link_path`，新字段可作为附加信息但不能替代旧字段。

```bash
curl --fail-with-body -H 'Authorization: Bearer API_KEY' \
  'https://HOST/api/v1/mail/latest'

curl --fail-with-body \
  'https://HOST/api/v1/mail/recent?api_key=API_KEY_OR_DIRECT_TOKEN'

curl --fail-with-body -X POST \
  -H "Authorization: Bearer ${ICLOUD_API_OAUTH_TOKEN}" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'add_hide_my_eamil=alias@icloud.com' \
  --data-urlencode 'icloud=primary@icloud.com' \
  'https://HOST/api/v1/aliases'
```

自动创建的 pending Key 仍通过管理 API 的 `GET` `<admin-path>/api/v1/accounts/{id}/aliases/auto-create/keys` 领取，并用同一路径的 `DELETE` 请求、提交 `alias_ids` 确认保存；读取不会删除队列，确认操作才会删除对应行。管理端兼容接口 `POST <admin-path>/api/v1/aliases/{id}/rotate-key` 只轮换 API Key；`POST <admin-path>/api/v1/aliases/{id}/rotate-credentials` 只用于 v2 alias 的整套凭证轮换。两者都只在显式调用后使对应旧凭据失效。上述 `<admin-path>/api/v1` 均可替换为固定兼容入口 `/admin/api/v1`。

## IMAPS 与令牌接口

IMAPS 用户名固定为隐私邮箱地址。可使用管理端显示的 IMAP 密码登录，也可先交换 access token，再使用 XOAUTH2 登录。

```bash
curl -X POST 'https://HOST/oauth2/v2.0/token' \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=refresh_token' \
  --data-urlencode 'client_id=CLIENT_ID' \
  --data-urlencode 'refresh_token=REFRESH_TOKEN'
```

响应采用标准 Bearer 字段：

```json
{
  "access_token": "ACCESS_TOKEN",
  "token_type": "Bearer",
  "expires_in": 3600
}
```

服务只提供一个只读 `INBOX`，支持 `LIST`、`STATUS`、`SELECT`、`EXAMINE`、`SEARCH`、`FETCH`、`UID FETCH` 和 `IDLE`。`APPEND`、`STORE`、`COPY`、`MOVE`、`DELETE`、`EXPUNGE` 等写操作返回 `READ-ONLY` 错误。

未显式配置生产证书时，服务会在 keys 卷中生成覆盖 `ICLOUD_API_PUBLIC_IMAP_SERVER_NAME` 的持久化自签证书。客户端必须信任该证书。生产环境应同时配置：

```text
ICLOUD_API_PUBLIC_IMAP_TLS_CERT_FILE=/path/to/fullchain.pem
ICLOUD_API_PUBLIC_IMAP_TLS_KEY_FILE=/path/to/private-key.pem
ICLOUD_API_PUBLIC_IMAP_SERVER_NAME=imap.example.com
```

证书的 SAN 必须覆盖 `ICLOUD_API_PUBLIC_IMAP_SERVER_NAME`。

## 生产部署

Compose 默认把 HTTP 和 IMAPS 都绑定在宿主机回环地址。对外服务时：

1. 用 HTTPS 反向代理转发 `127.0.0.1:8080`，保留外部 `Host`、`Origin` 和协议。
2. 设置 `ICLOUD_API_COOKIE_SECURE=true`，并把 `ICLOUD_API_TRUSTED_PROXIES` 收紧为实际代理地址或网段。
3. 通过防火墙受控开放 IMAPS；可把宿主机 `993` 映射到容器 `1993`，也可使用支持 TLS 透传的 TCP 代理。
4. 将生产证书和私钥通过只读 bind mount、Compose secret 或受保护的 keys 卷提供给容器，再设置证书路径与服务器名称。

Nginx 的 HTTP 反向代理至少保留：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $http_host;
    proxy_set_header Origin $http_origin;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

管理端复制的取码 URL 根据当前请求的外部主机名和协议生成，所以代理必须保留 `Host` 并正确传递 `X-Forwarded-Proto`。生产 IMAPS 应使用受客户端信任的证书；本地生成的自签证书适合回环和测试环境。

## 从 Outlook / Graph 客户端迁移

只需要验证码的调用方应直接改用管理端导出的五横线取码 URL；原先使用 Graph 获取完整邮件的调用方可迁移到只读 IMAPS。已经具备 Outlook IMAP OAuth2 分支的客户端通常只需：

- 把令牌地址改为本服务的 `/oauth2/v2.0/token`。
- 表单只发送 `grant_type`、`client_id`、`refresh_token`，移除 Microsoft `scope`。
- 把 IMAPS 主机改为部署方地址，用户名改为对应隐私邮箱。
- 使用本服务导出的 IMAP 密码，或继续使用标准 XOAUTH2 Bearer 格式。
- 用 `UIDVALIDITY + UID` 保存读取游标；归档邮箱为只读模型，`STORE \Seen` 返回 `READ-ONLY`。

四横线导出顺序与常见 Outlook 导入格式一致，但其中每项凭据都由本服务签发。Graph 文件夹、写操作和 deltaLink 逻辑应改为单一 `INBOX` 与本地稳定 UID 语义。

## 归档与留存

- 升级完成后只处理同步游标之后的新 UID；已读和未读邮件都会归档。
- 升级不会回填远端历史。v1 的最新快照只迁移标题和时间元数据，并分配稳定的本地 UID 1。
- PostgreSQL 保存邮件元数据、SHA-256、alias 映射、本地稳定 UID 和 OTP；原始 MIME 保存在独立 `icloud_api_mail_archive` 卷。
- 同一上游邮件投递给多个 alias 时只保存一份 MIME，各 alias 拥有自己的稳定邮箱 UID。
- `ICLOUD_API_MAIL_CONTENT_LIMIT_BYTES` 默认 `10737418240`（10 GiB）。每批提交后按收件时间全局 FIFO 淘汰最早正文，标题、时间、发件人、Message-ID、本地 UID 和历史记录永久保留。
- `ICLOUD_API_MAX_MESSAGE_BYTES` 默认且最高为 `104857600`（100 MiB）。超限邮件只保存元数据；抓取完整邮件时先流式写入临时文件，不把大 MIME 整体读入内存。
- 缺失、损坏、超限或已淘汰的内容在 IMAPS 中表现为说明性占位邮件，并保留原标题。

主要归档配置：

| 环境变量 | 默认值 | 说明 |
| --- | ---: | --- |
| `ICLOUD_API_PORT` | `8080` | Compose 的宿主机 HTTP 回环端口 |
| `ICLOUD_API_IMAPS_PORT` | `1993` | Compose 的宿主机 IMAPS 回环端口 |
| `ICLOUD_API_MAIL_CONTENT_LIMIT_BYTES` | `10737418240` | 全局 MIME 内容容量 |
| `ICLOUD_API_MAX_MESSAGE_BYTES` | `104857600` | 单封邮件硬上限，最大 100 MiB |
| `ICLOUD_API_MAX_BODY_BYTES` | `524288` | OTP/MIME 元数据解析时的正文预算 |
| `ICLOUD_API_PUBLIC_IMAP_ADDR` | `127.0.0.1:1993` | IMAPS 监听地址；Compose 中为 `0.0.0.0:1993` |
| `ICLOUD_API_PUBLIC_IMAP_SERVER_NAME` | `localhost` | IMAPS TLS 名称 |
| `ICLOUD_API_PUBLIC_IMAP_TLS_CERT_FILE` | 空（自动生成） | 生产证书在容器中的路径；证书和私钥需同时配置 |
| `ICLOUD_API_PUBLIC_IMAP_TLS_KEY_FILE` | 空（自动生成） | 生产私钥在容器中的路径 |
| `ICLOUD_API_POLL_INTERVAL` | `10s` | 自动同步周期 |
| `ICLOUD_API_IMAP_TIMEOUT` | `8s` | 单次上游 IMAP 操作时限 |
| `ICLOUD_API_SYNC_TIMEOUT` | `70s` | 单个账号同步时限 |
| `ICLOUD_API_SYNC_CONCURRENCY` | `3` | 同步并发数，范围 1–16 |
| `ICLOUD_API_SESSION_TTL` | `8h` | 管理端会话有效期 |
| `ICLOUD_API_COOKIE_SECURE` | `false` | HTTPS 生产部署应设为 `true` |
| `ICLOUD_API_TRUSTED_PROXIES` | 私有网段 | 受信反向代理 IP/CIDR，生产环境应收紧 |
| `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS` | `false` | 是否接受弱收件人头；保持默认值 |
| `TZ` | `Asia/Shanghai` | OTP API 输出时间所用时区 |

## v1 → v2 兼容升级

schema 升级后，服务会先校验主密钥，再按 alias ID 逐条事务补齐 v2 凭证包。迁移前已经领取的 alias 保持 `legacy` 模式，原 API Key、旧直达链接、消费记录和 IMAP `Seen` 任务不变；只有新 alias，或迁移前存在 pending 一次性 Key 但尚未领取的 alias，才会使用 v2 凭证。pending Key 会在补齐 v2 凭证时复用原 Key，不会静默替换。

升级会保留 `latest_messages`、`consumed_messages`、`imap_seen_tasks`、`pending_alias_api_keys` 及 `api_key_prefix`。归档表与旧快照双写，旧邮件路由继续读取兼容快照；安装级 OAuth Token 配置也继续可用于 `/api/v1/aliases`。如果曾部署过 PR2 的前序 v7 版本，应从管理端重新复制 v2 的 OTP 与 recent-mail URL，以换用用途隔离 token；迁移前 legacy alias 已保存的 v1 直达链接不受影响。升级从既有游标建立归档基线，远端历史邮件不会回填。

升级前必须同时备份 PostgreSQL、keys 卷和 mail archive 卷。不要只备份数据库：keys 卷包含主密钥、随机管理路径和本地 IMAPS 证书，mail archive 卷包含数据库所引用的 MIME 文件。使用卷快照时还应把 `postgres_data`、`postgres_config`、`installation_state`、`icloud_api_keys` 和 `icloud_api_mail_archive` 作为同一个恢复点；`postgres_socket` 属于临时通信卷。

以下逻辑备份流程先停止应用，避免归档文件与数据库在备份期间继续变化，PostgreSQL 保持运行以完成 `pg_dump`：

```bash
docker compose stop icloud-api

docker compose exec -T postgres \
  /usr/local/bin/icloud-api-postgres-entrypoint backup > postgres.dump

docker compose run --rm --no-deps \
  --entrypoint /usr/local/bin/icloud-api-keys-maintenance \
  icloud-api backup > keys.tar

docker compose run --rm --no-deps --entrypoint tar \
  icloud-api -C /app/mail-archive -cf - . > mail-archive.tar

docker compose start icloud-api
```

恢复或回滚时应停止 HTTP、IMAPS 和同步器，在同一个维护窗口中恢复三份匹配的备份。keys 维护器只接受已知文件，并校验管理路径以及本地 IMAPS 证书/私钥必须成对存在。回滚到 v1 代码时必须同时恢复升级前数据库，不能让 v1 直接读取 v2 schema。

不要执行 `docker compose down -v`。主密钥丢失后，Apple 会话与每个 alias 的完整凭证包都不能解密；mail archive 卷与数据库不匹配时，IMAPS 会将缺失内容降级为占位邮件。

升级完成后执行：

```bash
docker compose up -d --build --wait
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
docker compose exec -T icloud-api cat /app/keys/admin-path
```

然后验证旧 `/api/v1/mail/latest`、`/api/v1/mail/recent` 和 `/api/v1/aliases` 调用仍可用，已有 legacy Key 和直达链接仍能读取/消费邮件；再为新 alias 分发 OTP 或 IMAP/OAuth 凭据，并验证新邮件能够通过两套接口读取。

## 本地开发验证

本仓库使用 Go 1.26、Node.js 22。常用命令：

```bash
go test ./...
go vet ./...
go build ./cmd/icloud-api

cd web
npm ci
npm test
npm run build
```

集成回归应使用隔离的 iCloud 测试账号和真实 Apple IMAP 端点；生产进程不会接受测试 IMAP 地址、TLS 名称或 CA 覆盖。

## 安全与运行注意事项

- 生产环境保持单个 `icloud-api` 实例，避免拆分进程内账号锁、同步调度和限流状态。
- 管理端、部署级 OAuth token、API Key、取码 URL、IMAP 密码、refresh token、主密钥、App 专用密码和备份都按敏感凭据管理。
- URL 可能进入浏览器历史、代理日志和监控系统；legacy alias 可轮换 API Key 使旧直达链接失效，v2 alias 应轮换整套凭证。
- 原始 MIME、主题和 HTML 都属于外部输入；展示 HTML 前应清理内容或放入严格隔离的沙箱。
- 邮件归属依赖 iCloud 转发链路中的收件人头。保持 `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS=false`，并用真实 Hide My Email 样本验收路由。
- 在边缘代理设置请求速率、连接数和正文大小限制，限制管理路径的来源网络，并定期演练 PostgreSQL、keys 与 mail archive 的成组恢复。
