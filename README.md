# iCloud 隐私邮箱收件 API

一个自托管的 iCloud Hide My Email 收件服务。项目使用 Go、Gin、Vue 3、Element Plus 和 PostgreSQL，提供隐私邮箱管理、未读邮件同步、独立 API Key、邮件查询接口和管理后台。

## 免责声明

> [!WARNING]
> 本项目是非官方工具，与 Apple Inc. 没有隶属、授权或背书关系。项目依赖 iCloud IMAP 及可能随时变化的 Apple Web 接口；自动登录、目录同步、自动创建或删除隐私邮箱等操作可能触发验证码、限流、风控或服务条款限制，严重时可能导致 Apple ID（Apple 账户）功能受限、锁定、暂停、封禁或终止，并影响对 iCloud 邮件、文件、照片、备份、订阅、购买项目或设备服务的访问。
>
> 使用者应自行确认其用途符合 Apple 服务条款及适用法律，并自行承担账号、隐私邮箱、邮件、凭据或其他数据丢失，以及服务中断和其他直接或间接损失。项目按“现状”提供，作者和贡献者不对可用性、兼容性、数据完整性或任何损失作出保证或承担赔偿责任。请仅操作本人或已获明确授权的账户，优先使用能够承受风险的测试账户，提前做好备份，不要将本服务作为重要邮件的唯一接收或存储渠道。

## 功能

- 管理多个 iCloud 主号，每个主号可绑定多个隐私邮箱。
- 通过 iCloud IMAP/TLS 同步主号收件箱中的未读邮件。
- 从 Apple 账户同步隐私邮箱目录，并可按主号选择性自动创建地址。
- 为每个隐私邮箱生成独立 API Key，Key 之间相互隔离。
- 提供可重复读取的完整邮件接口和一次性消费的紧凑接口。
- 提供 Vue 管理后台、同步进度、错误日志和操作审计。
- PostgreSQL 持久化主号、隐私邮箱、最新邮件快照、同步游标和审计记录。

```text
iCloud 主号（一个 IMAP 收件箱）
  ├─ 隐私邮箱 A ─ API Key A ─ 只读取 A 的最新邮件
  ├─ 隐私邮箱 B ─ API Key B ─ 只读取 B 的最新邮件
  └─ 隐私邮箱 C ─ API Key C ─ 只读取 C 的最新邮件
```

## 使用前须知

- 本项目只保存每个隐私邮箱的最新一封邮件快照，不是邮件归档系统。
- 邮件同步只处理主号 `INBOX` 中的未读邮件。其他客户端若先把邮件标为已读，本服务可能不会再同步该邮件。
- 完整接口可重复读取最新快照；紧凑接口只消费最近一小时内的当前快照，并异步将对应邮件标为已读。
- 新建或轮换 API Key 时，原始 Key 只显示一次。丢失后只能重新轮换。
- 永久删除隐私邮箱会同时删除 Apple 端地址，操作不可恢复。
- 生产环境只运行一个 `icloud-api` 实例，多实例会重复轮询并拆分进程内同步锁和限流状态。

## 快速启动

### 1. 准备环境

需要：

- Docker Engine
- Docker Compose v2
- 已启用双重认证的 Apple 账户
- 用于 iCloud IMAP 的 App 专用密码

登录 [Apple 账户管理页](https://account.apple.com/)，在“登录与安全性”中创建 App 专用密码。IMAP 不接受 Apple 账户登录密码代替 App 专用密码。

### 2. 启动服务

新部署无需创建 `.env`：

```bash
docker compose up -d --build --wait
docker compose ps

APP_ADDR="$(docker compose port icloud-api 8080)"
curl -fsS "http://${APP_ADDR}/healthz"
```

默认管理后台为 <http://127.0.0.1:8080/admin/>，默认管理员用户名为 `admin`。

首次启动会自动生成管理员密码、外部登记接口的 OAuth Token 和主密钥。查看前两项：

```bash
MSYS_NO_PATHCONV=1 docker compose exec -T icloud-api cat /app/keys/admin-password
MSYS_NO_PATHCONV=1 docker compose exec -T icloud-api cat /app/keys/oauth-token
```

这些值属于敏感凭据。不要提交到版本库，也不要发送到聊天、工单或普通日志中。

查看应用日志：

```bash
docker compose logs -f icloud-api
```

### 3. 添加 iCloud 主号

1. 登录管理后台，添加主号。
2. 填写主号邮箱、IMAP 用户名和 App 专用密码。IMAP 用户名通常是完整的 iCloud 邮箱地址。
3. 点击“同步邮件”验证 IMAP 连接。
4. 在主号详情页手动登记隐私邮箱，或点击“同步隐私邮箱”读取 Apple 账户的完整目录，并导入其中转发到当前主号的地址。
5. 立即保存新地址仅显示一次的 API Key。

目录同步、自动创建和永久删除需要使用 Apple 账户密码及 6 位双重认证验证码建立受信任 Web 会话。账户密码和验证码不写入数据库，服务仅加密保存会话材料；会话过期后需要重新登录。

### 4. 自动创建隐私邮箱（可选）

主号详情页可独立开启“自动创建隐私邮箱”。开启后，服务每 60 分钟最多尝试创建 5 次，每次间隔至少 5 分钟；新 Key 会进入“待领取”队列。

自动创建依赖 Apple Web 接口，可能遇到验证、限流、会话失效或接口变更。启用前请阅读顶部免责声明，并确认 Apple 端转发目标是当前主号。

## 收件 API

完整接口定义、请求模型和错误响应见 [`docs/openapi.yaml`](docs/openapi.yaml)。每个隐私邮箱使用自己的 `icm_` API Key，调用方不能通过参数改查其他邮箱。

| 接口 | 鉴权 | 行为 |
| --- | --- | --- |
| `GET /api/v1/mail/latest` | `Authorization: Bearer <API Key>` | 可重复读取当前最新快照，不消费邮件，不修改已读状态 |
| `GET /api/v1/mail/recent` | URL 查询参数 `api_key` | 只返回最近一小时内尚未消费的当前快照；成功后记录消费并异步标记已读 |
| `POST /api/v1/aliases` | `Authorization: Bearer <OAuth Token>` | 登记已经在 Apple 端存在的隐私邮箱，不负责创建地址 |

### 完整邮件接口（推荐）

```bash
curl --fail-with-body \
  -H 'Authorization: Bearer icm_REPLACE_WITH_ALIAS_KEY' \
  https://mail.example.com/api/v1/mail/latest
```

响应包含发件人、收件人、主题、纯文本、HTML、附件元数据和同步时间。该接口可以重复读取同一 `message.id`，不会触发一次性消费或已读标记。

### 紧凑直达接口

```text
https://mail.example.com/api/v1/mail/recent?api_key=icm_REPLACE_WITH_DIRECT_TOKEN
```

成功响应示例：

```json
{
  "data": {
    "address": "alias@icloud.com",
    "subject": "Your verification code",
    "snippet": "Your code is 123456",
    "sent_at": "2026-08-10T12:30:00+08:00"
  }
}
```

管理后台会提供可复制的派生直达链接。完整链接必须按密钥保护：URL 可能进入浏览器历史、代理日志和监控系统，链接预览、浏览器预取或安全扫描也可能提前消费邮件。能设置请求头的客户端应优先使用完整邮件接口。

### 外部登记接口

此接口只登记已经由 Apple 创建并启用的隐私邮箱：

```bash
curl --fail-with-body \
  -X POST \
  -H "Authorization: Bearer ${ICLOUD_API_OAUTH_TOKEN}" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'add_hide_my_eamil=alias@icloud.com' \
  --data-urlencode 'icloud=primary@icloud.com' \
  https://mail.example.com/api/v1/aliases
```

`add_hide_my_eamil` 是既有兼容字段名，请保持该拼写。成功响应中的 `api_key` 只显示一次。

### 常见状态码

| 状态码 | 含义 |
| --- | --- |
| `200` / `201` | 查询或登记成功 |
| `401` | API Key 或 OAuth Token 缺失、格式错误、无效，或对应账号已停用 |
| `404` | 没有可返回的邮件、邮件已消费，或主号不存在 |
| `409` | 隐私邮箱已登记或达到容量上限 |
| `429` | 请求过于频繁 |
| `503` | 邮件仍在同步、最近同步失败、快照已过期或数据库暂不可用 |

健康检查无需鉴权：

```bash
curl -fsS https://mail.example.com/healthz
```

## 常用配置

可在项目根目录创建 `.env` 覆盖 Compose 默认值，只填写确实需要修改的项目：

```dotenv
ICLOUD_API_PORT=8080
ICLOUD_API_ADMIN_USER=admin
ICLOUD_API_ADMIN_PASSWORD=请替换为至少12位的高强度密码
ICLOUD_API_OAUTH_TOKEN=请替换为32至4096个无空白字符的随机令牌
ICLOUD_API_MASTER_KEY=请粘贴32字节密钥的Base64或十六进制编码
# 通过 HTTPS 反向代理对外服务时改为 true
ICLOUD_API_COOKIE_SECURE=false
ICLOUD_API_POLL_INTERVAL=1m
ICLOUD_API_SYNC_TIMEOUT=10m
TZ=Asia/Shanghai
```

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ICLOUD_API_PORT` | `8080` | 宿主机回环监听端口 |
| `ICLOUD_API_ADMIN_USER` | `admin` | 首次初始化或管理员重置时使用的用户名 |
| `ICLOUD_API_ADMIN_PASSWORD` | 自动生成 | 管理员密码，长度为 12 至 72 字节 |
| `ICLOUD_API_OAUTH_TOKEN` | 自动生成 | 外部登记接口令牌，不得与其他凭据复用 |
| `ICLOUD_API_MASTER_KEY` | 自动生成 | 加密 IMAP 凭据、Apple 会话并签名直达凭据 |
| `ICLOUD_API_COOKIE_SECURE` | `false` | 通过 HTTPS 对外服务时应设为 `true` |
| `ICLOUD_API_POLL_INTERVAL` | `1m` | IMAP 轮询间隔，范围 `10s` 至 `24h` |
| `ICLOUD_API_IMAP_TIMEOUT` | `25s` | 单次 IMAP 操作超时 |
| `ICLOUD_API_SYNC_TIMEOUT` | `10m` | 单个主号完整同步预算，至少为 IMAP 超时的两倍 |
| `ICLOUD_API_SYNC_CONCURRENCY` | `3` | 同时同步的主号数，范围 `1` 至 `16` |
| `ICLOUD_API_TRUSTED_PROXIES` | Compose 默认私有网段 | 受信反向代理 IP 或 CIDR，生产环境建议收紧 |
| `TZ` | `Asia/Shanghai` | 紧凑接口返回时间所用时区 |

修改 `.env` 中的管理员用户名或密码不会覆盖数据库中的现有管理员。需要显式重置时运行：

```bash
docker compose run --rm --no-deps icloud-api admin reset
```

## 反向代理

Compose 默认只把端口发布到 `127.0.0.1`。通过 Nginx、1Panel 或其他反向代理提供服务时，应使用 HTTPS、保留外部主机名和协议，并设置 `ICLOUD_API_COOKIE_SECURE=true`。

Nginx 至少保留以下请求头：

```nginx
proxy_set_header Host $http_host;
proxy_set_header Origin $http_origin;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
```

不要删除或改写浏览器发送的 `Origin`，否则管理后台登录和写操作可能被同源安全检查拒绝。

## 从源码运行

需要 Go 1.26、Node.js 22 和可用的 PostgreSQL 实例。

启动后端：

```bash
export ICLOUD_API_ADMIN_USER=admin
export ICLOUD_API_ADMIN_PASSWORD='请替换为至少12位的高强度密码'
export ICLOUD_API_MASTER_KEY='请粘贴32字节密钥的Base64或十六进制编码'
export ICLOUD_API_OAUTH_TOKEN='请替换为32至4096个无空白字符的随机令牌'
export ICLOUD_API_DATABASE_URL='postgres://icloud_api:数据库密码@127.0.0.1:5432/icloud_api?sslmode=disable'
export ICLOUD_API_ADDR=127.0.0.1:8080
go run ./cmd/icloud-api
```

另开终端启动管理端开发服务器：

```bash
cd web
npm ci
npm run dev
```

开发管理端默认位于 <http://127.0.0.1:5173/admin/>。生产环境建议使用 Docker Compose 构建，不需要单独运行前端服务。

## 数据与备份

需要作为同一恢复点备份的命名卷：

- `postgres_data`：业务数据。
- `postgres_config`：数据库随机凭据和安装标识。
- `installation_state`：数据库与主密钥的绑定状态。
- `icloud_api_keys`：主密钥、自动生成的管理员密码和 OAuth Token。

`postgres_socket` 只用于容器间通信，无需备份。执行存储平台的卷快照前，应停止应用和 PostgreSQL，并将上述四个卷作为同一恢复点处理；也可以在维护窗口中同时制作 PostgreSQL 逻辑备份和应用密钥归档。恢复后务必验证管理员登录、凭据解密、IMAP 同步和邮件 API。

通过 `.env` 或 Secret 显式提供的主密钥、管理员密码和 OAuth Token 不会完整保存到 `icloud_api_keys`。这些值必须单独安全备份、纳入同一恢复集，并在恢复时重新注入原值。

不要执行 `docker compose down -v`，它会删除命名卷。主密钥丢失后，已保存的 IMAP 凭据和 Apple 会话将无法解密，已复制的派生直达链接也会失效。

## 安全事项

- 生产环境必须使用 HTTPS，限制管理后台来源 IP，并设置 `ICLOUD_API_COOKIE_SECURE=true`。
- App 专用密码、Apple 会话、API Key、OAuth Token、主密钥、数据库和备份均属于敏感信息，应限制访问并避免写入日志。
- 怀疑邮件 API Key 或直达链接泄露时立即轮换 Key；App 专用密码泄露时应在 Apple 账户页面撤销并更新主号凭据。
- `/api/v1/mail/recent` 会产生消费和全局已读副作用，不要把链接交给会自动访问 URL 的系统。
- 完整接口返回的 `html` 是不可信外部内容，展示前必须清理或放入严格隔离的沙箱，禁止直接插入页面 DOM。
- 邮件归属依赖 Apple 转发链路注入的 `X-ICLOUD-HME`、`Delivered-To`、`X-Original-To` 等普通邮件头，并非密码学证明。上线前应使用真实 Hide My Email 原始邮件验证正常投递和发件人预置同名头的场景；即使使用默认配置，也仍有邮件误归属的残余风险。
- 保持 `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS=false`。只有在真实邮件样本验收并明确接受邮件误归属风险后才考虑启用。
- 在反向代理层设置请求大小、速率和连接数限制；应用内限流不能替代边缘限流。
- 定期备份并进行恢复演练，重要邮件应继续保留在可靠的邮件或归档系统中。
