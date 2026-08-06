# iCloud 隐私邮箱收件 API

这是一个使用 Go、Gin、iCloud IMAP 和 SQLite 构建的轻量收件服务。后台可以登记 iCloud 主号，并为主号绑定多个“隐藏邮件地址”（隐私邮箱）；服务定时通过该主号的同一个 IMAP 收件箱同步邮件。每个隐私邮箱拥有独立 API Key，调用者只能读取这个 Key 所属隐私邮箱的最新一封邮件。

## 权限与数据模型

```text
iCloud 主号（一个 IMAP 连接）
  ├─ 隐私邮箱 A ─ 独立 Key A ─ 仅返回 A 的最新邮件
  ├─ 隐私邮箱 B ─ 独立 Key B ─ 仅返回 B 的最新邮件
  └─ 隐私邮箱 C ─ 独立 Key C ─ 仅返回 C 的最新邮件
```

- 一个主号可以绑定多个隐私邮箱；隐私邮箱收到的邮件应已由 Apple 转发到这个主号的 `INBOX`。
- API 请求不接收邮箱地址参数。服务先对 Bearer Key 做哈希匹配，再从数据库中读取与该 Key 绑定的唯一隐私邮箱，因此 Key A 无权指定或读取邮箱 B。
- 每个隐私邮箱只保存一条最新邮件快照。同步到更新邮件时，旧快照会被替换；本项目不是邮件归档系统。
- 新建或轮换 Key 时，完整 Key 只在后台显示一次。数据库只保存 Key 哈希和用于辨认的前缀。
- 禁用主号或隐私邮箱后，对应 Key 立即失效；轮换 Key 后旧 Key 立即失效。

完整 API Key 是 47 个 ASCII 字符：固定小写前缀 `icm_`，后接 43 位无 `=` 填充的规范 Raw Base64URL，严格解码后为 32 字节。规范格式可表示为：

```text
^icm_[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$
```

最后一位的受限字符集用于保证末尾填充位为零；服务端还会执行严格 Base64URL 解码和 32 字节长度检查。标准 Base64 的 `+`、`/`、尾部 `=`、空白、长度不符和非规范末位都会得到 `401 INVALID_API_KEY`。

## 准备 iCloud

### 1. 创建 App 专用密码

iCloud IMAP 不应使用 Apple 账户登录密码。先确认 Apple 账户已启用双重认证，然后：

1. 登录 [Apple 账户管理页](https://account.apple.com/)。
2. 进入“登录与安全性”中的“App 专用密码”。
3. 为本服务生成一个新的 App 专用密码，并妥善记录。
4. 以后停止使用本服务或怀疑凭据泄露时，在同一页面撤销该密码。

App 专用密码只用于后台的 IMAP 密码字段。IMAP 连接固定使用 `imap.mail.me.com:993` 和 TLS，IMAP 用户名通常是完整的 iCloud 主号邮箱地址。

### 2. 确认隐私邮箱归属

本服务不会通过 Apple 接口创建“隐藏邮件地址”。请先在 Apple/iCloud 中创建并启用隐私邮箱，确认其邮件转发到准备登记的主号，然后在本服务后台逐个登记这些地址。一个隐私邮箱只能登记一次，也只能归属于一个主号。

## 使用 Docker Compose 启动

Docker 部署会使用非 root 用户运行进程，根文件系统为只读，并将 SQLite 数据库保存在持久化卷中。凭据主密钥必须通过环境变量单独提供，并与数据卷分离。项目不依赖 Redis 或 PostgreSQL。

在项目目录创建本机专用的 `.env`：

```dotenv
ICLOUD_API_ADMIN_USER=admin
ICLOUD_API_ADMIN_PASSWORD=请替换为至少12位的高强度密码
ICLOUD_API_MASTER_KEY=请粘贴32字节Base64主密钥
ICLOUD_API_PORT=8080
ICLOUD_API_COOKIE_SECURE=false
# 单个主号一整轮同步的总时限，合法范围为 10s 到 30m
ICLOUD_API_SYNC_TIMEOUT=2m
TZ=Asia/Shanghai
```

可用 `go run ./cmd/icloud-api keygen` 或 `openssl rand -base64 32` 生成主密钥并填入 `.env`。Compose 将 `ICLOUD_API_MASTER_KEY` 作为必填项，应用直接从环境变量读取它，不会把主密钥生成或写入 `/app/data`。重启、迁移和恢复时必须注入原来的同一把密钥。

`.env` 包含登录凭据和主密钥，不要提交到版本库，也不要放入镜像。生产环境优先通过部署平台的 Secret 机制注入主密钥。若经由 HTTPS 反向代理对外提供后台，请把 `ICLOUD_API_COOKIE_SECURE` 设为 `true`。

构建并启动：

```bash
docker compose up -d --build
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
```

后台地址为 `http://127.0.0.1:8080/admin`。查看启动日志：

```bash
docker compose logs -f icloud-api
```

生产部署只运行一个服务副本。禁止使用 `docker compose up --scale icloud-api=2`，也不要让多个进程或多台主机共享同一个 SQLite 文件；当前同步调度、进程内限流和 SQLite 写入模型均按单副本设计。

## 从源码启动

需要 Go 1.26 或与 `go.mod` 兼容的更新版本。首次启动前至少设置管理员密码：

```bash
export ICLOUD_API_ADMIN_USER=admin
export ICLOUD_API_ADMIN_PASSWORD='请替换为至少12位的高强度密码'
export ICLOUD_API_MASTER_KEY='请粘贴32字节Base64主密钥'
export ICLOUD_API_ADDR=127.0.0.1:8080
go run ./cmd/icloud-api
```

首次初始化必须设置 `ICLOUD_API_ADMIN_PASSWORD`，应用不会生成或记录明文管理员密码。管理员用户名和密码只在数据库中没有管理员的首次启动时用于初始化；已有数据库不会因为修改环境变量而修改现有登录凭据。

服务会自动创建 SQLite 表结构。原生运行时，如果没有设置 `ICLOUD_API_MASTER_KEY`，应用才会在 `ICLOUD_API_MASTER_KEY_FILE` 指定的位置生成本地主密钥文件；Compose 部署始终使用环境变量中的主密钥。也可以先构建二进制：

```bash
go build -trimpath -o icloud-api ./cmd/icloud-api
./icloud-api
```

## 环境变量

Go 时长使用 `10s`、`1m`、`8h` 这类格式；正文大小使用字节数。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ICLOUD_API_ADDR` | `127.0.0.1:8080` | HTTP 监听地址；容器内设置为 `0.0.0.0:8080` |
| `ICLOUD_API_DB` | `data/icloud-api.db` | SQLite 文件路径 |
| `ICLOUD_API_MASTER_KEY_FILE` | `<数据库路径>.key` | 仅用于原生运行的文件回退；Compose 不设置此项 |
| `ICLOUD_API_MASTER_KEY` | 空 | 32 字节 Base64 或十六进制主密钥；Compose 部署必填，设置后优先于文件 |
| `ICLOUD_API_ADMIN_USER` | `admin` | 首次初始化的管理员用户名 |
| `ICLOUD_API_ADMIN_PASSWORD` | 空 | 首次初始化的管理员密码，长度为 12 到 72 字节；Compose 部署必填 |
| `ICLOUD_API_COOKIE_SECURE` | `false` | 是否只允许浏览器通过 HTTPS 发送后台会话 Cookie |
| `ICLOUD_API_SESSION_TTL` | `8h` | 后台登录会话有效期 |
| `ICLOUD_API_POLL_INTERVAL` | `1m` | IMAP 轮询间隔，最短 `10s` |
| `ICLOUD_API_IMAP_TIMEOUT` | `25s` | 单次 IMAP 网络操作超时 |
| `ICLOUD_API_SYNC_TIMEOUT` | `2m` | 单个主号一整轮同步的总时限，范围 `10s` 到 `30m` |
| `ICLOUD_API_SYNC_CONCURRENCY` | `3` | 同时同步的主号数，范围 `1` 到 `16` |
| `ICLOUD_API_SHUTDOWN_TIMEOUT` | `10s` | 收到退出信号后的优雅关闭时限 |
| `ICLOUD_API_MAX_MESSAGE_BYTES` | `10485760` | 单封邮件最多读取的原始字节数，默认 10 MiB |
| `ICLOUD_API_MAX_BODY_BYTES` | `1048576` | 解析后正文总量上限，默认 1 MiB |
| `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS` | `false` | 是否在缺少投递头时允许用 `To`/`Cc` 归属邮件；详见安全事项 |
| `ICLOUD_API_TRUSTED_PROXIES` | 空 | 逗号分隔的受信反向代理 IP/CIDR；仅填写实际代理地址，用于恢复客户端 IP 和限流 |
| `GIN_MODE` | `release` | Gin 运行模式 |
| `TZ` | 系统时区 | 后台页面的本地时间显示；API 时间始终使用 RFC 3339/UTC |

主密钥也可以用内置命令生成：

```bash
go run ./cmd/icloud-api keygen
```

数据库中的 IMAP App 专用密码使用主密钥进行 AES-GCM 加密。主密钥一旦丢失，现有 IMAP 密文将失去解密条件。Compose 数据卷不保存主密钥，必须从 Secret 管理系统或受保护的 `.env` 单独备份，并与对应数据库组成同一份恢复集。

## 后台操作

1. 打开 `/admin/login`，使用首次启动时配置的管理员账户登录。
2. 添加主号，填写名称、iCloud 主号邮箱、IMAP 用户名和 App 专用密码。
3. 进入主号详情页，添加所有归属于该主号的隐私邮箱地址。每次创建都会生成一个独立 API Key，请立即保存。
4. 使用“立即同步”检查 IMAP 连接和收件状态；后台也会按轮询间隔自动同步所有已启用主号。
5. 在隐私邮箱页面查看归属、状态和最近收件时间。需要时可以停用、删除或轮换 Key。
6. 在操作记录页面检查登录、配置变更、同步和 Key 轮换记录。

轮换 Key 会直接吊销旧 Key。后台不会再次显示任何已经离开创建结果页的完整 Key。

## 收件 API

完整接口定义见 [`docs/openapi.yaml`](docs/openapi.yaml)。唯一的收件接口是：

```http
GET /api/v1/mail/latest
Authorization: Bearer icm_<后台显示的43位RawBase64URL密钥>
```

尖括号内容是说明性占位符，上面这行并非可通过格式校验的真实 Key。调用时需要用后台创建或轮换隐私邮箱时显示的完整 Key 替换整个占位值。

调用示例中的 `icm_REPLACE_WITH_ALIAS_KEY` 同样是故意设置为格式无效的占位值，直接发送会得到 `401 INVALID_API_KEY`：

```bash
curl --fail-with-body \
  -H 'Authorization: Bearer icm_REPLACE_WITH_ALIAS_KEY' \
  https://mail.example.com/api/v1/mail/latest
```

成功响应示例：

```json
{
  "data": {
    "alias": "example@icloud.com",
    "message": {
      "id": "123456-789",
      "message_id": "<message@example.com>",
      "received_at": "2026-08-06T02:30:00Z",
      "sent_at": "2026-08-06T02:29:58Z",
      "from": [
        {"name": "Example", "email": "sender@example.com"}
      ],
      "to": [
        {"email": "example@icloud.com"}
      ],
      "cc": [],
      "subject": "验证码",
      "text": "您的验证码是 123456",
      "html": "<p>您的验证码是 <strong>123456</strong></p>",
      "attachments": [],
      "has_attachments": false,
      "body_truncated": false
    },
    "synced_at": "2026-08-06T02:30:12Z",
    "stale": false
  }
}
```

`id` 由 IMAP `UIDVALIDITY` 和 `UID` 组成，可以用于判断两次读取是否仍为同一封邮件。`sent_at` 来自邮件头，缺失时为 `null`；`received_at` 来自 IMAP 服务器。`body_truncated=true` 表示邮件或正文超过配置上限，返回内容可能不完整。附件只返回文件名、类型和大小元数据，不返回附件内容。

常见状态码：

| 状态码 | 错误码 | 含义 |
| --- | --- | --- |
| `401` | `INVALID_API_KEY` | Key 不符合 `icm_` 加 43 位规范 Raw Base64URL、内容未知，或者对应主号/隐私邮箱已停用 |
| `404` | `MAIL_NOT_FOUND` | 该隐私邮箱最近同步状态为 `ok`、结果仍在有效期内，并确认当前没有邮件 |
| `429` | `RATE_LIMITED` | 单个 Key 请求过于频繁；当前进程内限制为每分钟 120 次 |
| `503` | `SYNC_UNAVAILABLE` | 该隐私邮箱为 `pending`/`error`、从未完成同步，或结果已超过三个轮询周期 |
| `503` | `DATABASE_UNAVAILABLE` | 查询 Key 或最新邮件快照时数据库暂不可用 |

错误响应统一包含错误码、说明和请求编号：

```json
{
  "error": {
    "code": "MAIL_NOT_FOUND",
    "message": "尚未收到邮件",
    "request_id": "REQUEST_ID"
  }
}
```

健康检查不需要鉴权：

```bash
curl -fsS https://mail.example.com/healthz
# {"status":"ok"}
```

## 轮询与“仅最新一条”语义

- API 读取 SQLite 快照，不会在每次 API 调用时连接 iCloud；因此延迟通常为一个 `ICLOUD_API_POLL_INTERVAL`，外加 IMAP 网络耗时。
- 每轮同步按主号建立一个只读 IMAP 连接，并处理这个主号下的所有已启用隐私邮箱。
- 同步可用性按隐私邮箱独立判断：同一主号下一个地址状态未确认时，该地址返回 `503`；其他已确认地址仍可正常返回 `200` 或 `404`。
- 服务从 iCloud 转发邮件中的候选投递收件头精确解析邮箱地址，不做字符串包含匹配；这些普通邮件头的可信程度取决于下文所述的 iCloud 转发行为。单个主号默认最多处理 256 个已启用隐私邮箱。
- SQLite 对每个隐私邮箱只保留一行最新邮件。新 UID 会替换旧 UID，旧邮件不会通过 API 查询。
- `pending` 表示地址刚创建、重新启用或因主号凭据变化而重置，尚待一次可确认的同步；API 返回 `503`。
- `error` 表示最近同步没有确认该地址的最新状态。旧快照可能仍保存在内部，但 API 会返回 `503`，不会把它作为可用邮件返回。
- `ok` 表示最近同步已明确找到最新邮件，或权威确认当前为空。在三个轮询周期的有效期内，有快照返回 `200`，无快照返回 `404`；超过有效期后返回 `503`。
- 当前版本的成功响应中 `stale` 始终为 `false`。降级状态统一返回 `503`，不会返回带 `stale=true` 的旧快照。
- 客户端可保存 `message.id` 并定时重试；ID 未变化时不要重复处理。建议轮询频率低于服务端每分钟 120 次的 Key 限流。

## 备份与恢复

Compose 使用命名卷 `icloud_api_data` 保存 `/app/data`。该卷只包含 SQLite 数据库及可能存在的 `-wal`/`-shm` 辅助文件，不包含主密钥。为得到一致备份，先停止唯一的服务副本，再复制整个数据目录：

```bash
mkdir -p backup
docker compose stop icloud-api
docker compose cp --archive icloud-api:/app/data ./backup/icloud-api-data
docker compose start icloud-api
```

恢复前同样先停止服务。下面的清理命令只针对 Compose 中固定的 `/app/data` 数据文件；先确认备份完整，再执行恢复。清空旧文件可以避免旧 WAL 与备份数据库混用，`--archive` 用于保留非 root 运行用户的文件所有权：

```bash
docker compose stop icloud-api
docker compose run --rm --no-deps --entrypoint sh icloud-api -c \
  'rm -f /app/data/icloud-api.db /app/data/icloud-api.db-shm /app/data/icloud-api.db-wal'
docker compose cp --archive ./backup/icloud-api-data/. icloud-api:/app/data/
docker compose start icloud-api
curl -fsS http://127.0.0.1:8080/healthz
```

启动恢复实例前，先通过 `ICLOUD_API_MASTER_KEY` 注入这份数据库原来使用的主密钥。数据库目录备份本身不携带该密钥。

恢复后若容器因数据目录权限报错，说明备份所在文件系统没有保留 UID/GID 元数据；应在受控维护窗口中把 `/app/data` 的所有者修正为镜像内的 `10001:10001`，再重新启动服务。

原生部署也应在进程停止后备份 `ICLOUD_API_DB` 及同目录下的 `-wal`/`-shm` 文件（若存在）。使用文件回退时，同时备份 `ICLOUD_API_MASTER_KEY_FILE`；使用 `ICLOUD_API_MASTER_KEY` 时，则从密钥管理系统单独备份该值。

## 安全事项

- 生产环境应放在 HTTPS 反向代理之后，并设置 `ICLOUD_API_COOKIE_SECURE=true`。限制后台来源 IP，不要直接把无 TLS 的管理端口暴露到公网。
- `.env`、App 专用密码、完整 API Key、SQLite 数据和主密钥都属于敏感信息。限制文件/卷权限，不要写入代码、URL、工单或普通访问日志。
- API 返回的 `html` 是外部邮件内容。前端展示时必须做 HTML 清理或放入严格隔离的沙箱，禁止直接插入管理页面 DOM。
- 默认使用 `Delivered-To`、`X-Original-To`、`Envelope-To` 等投递头判断邮件归属。这些都是普通邮件头，本身没有密码学可信性；归属隔离依赖 iCloud 在实际 Hide My Email 转发链路中注入可识别的隐私邮箱投递标记，并清洗或隔离发件人预置的同名头。
- 上线前必须用真实 Hide My Email 原始邮件完成验收：既测试正常投递，也测试发件人预置同名 `Delivered-To`、`X-Original-To`、`Envelope-To` 等头的投递，确认 iCloud 最终保存的原始邮件仍能让服务唯一识别真实隐私邮箱。不同候选投递头指向不同地址时，服务会 fail-closed，将该地址视为未确认并返回 `503`，不会返回旧快照；如果 Apple 没有注入可识别标记且保留了单个伪造同名头，仍存在邮件误归属的残余风险。
- `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS` 默认为 `false`。启用后会在缺少上述投递标记时使用可由发件人影响的 `To`/`Cc`，会进一步降低隐私邮箱隔离强度；只有在真实样本验收并明确接受该风险后才应启用。
- Key 泄露后立即在后台轮换；App 专用密码泄露后应在 Apple 账户页面撤销，再在后台更新主号凭据。
- 应同时在反向代理层设置请求大小、速率和连接数限制。应用内 Key 限流保存在单个进程内，不替代边缘限流。
- 生产环境维持单副本运行。多个实例会重复轮询 IMAP、拆分进程内限流，并对共享 SQLite 产生竞争。
- 定期做停机一致性备份并执行恢复演练。确认恢复集同时包含数据库备份与单独保管的对应主密钥。
