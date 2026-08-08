# iCloud 隐私邮箱收件 API

这是一个使用 Go、Gin、Vue 3、Element Plus、iCloud IMAP 和 PostgreSQL 构建的轻量收件服务。Vue 管理端可以登记 iCloud 主号，并为主号绑定多个“隐藏邮件地址”（隐私邮箱）；Go API 服务定时通过该主号的同一个 IMAP 收件箱同步邮件。每个隐私邮箱拥有独立 API Key，调用者只能读取这个 Key 所属隐私邮箱的最新邮件快照；可直接访问的紧凑链接只在该快照属于最近一小时时返回邮件。

## 权限与数据模型

```text
iCloud 主号（一个 IMAP 连接）
  ├─ 隐私邮箱 A ─ 独立 Key A ─ 仅返回 A 的最新邮件
  ├─ 隐私邮箱 B ─ 独立 Key B ─ 仅返回 B 的最新邮件
  └─ 隐私邮箱 C ─ 独立 Key C ─ 仅返回 C 的最新邮件
```

- 一个主号可以绑定多个隐私邮箱；隐私邮箱收到的邮件应已由 Apple 转发到这个主号的 `INBOX`。
- 邮件同步以主号为单位建立一条只读 IMAP 连接，并使用 PostgreSQL 中保存的 `UIDVALIDITY + last_uid` 游标读取新增 UID；隐私邮箱数量不会转换成同等数量的 iCloud 查询。
- 收件 API 请求不接收邮箱地址参数。服务从 Bearer Header 或紧凑接口的 `api_key` 查询参数取得 Key，做哈希匹配后读取与该 Key 绑定的唯一隐私邮箱，因此 Key A 无权指定或读取邮箱 B。外部登记接口使用独立的 OAuth Bearer 令牌，并接收待登记隐私邮箱和所属主号邮箱。
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

App 专用密码只用于后台的 IMAP 密码字段。邮件正文、文件夹和已读/收件状态始终通过 `imap.mail.me.com:993` 和 TLS 获取，IMAP 用户名通常是完整的 iCloud 主号邮箱地址。Apple 账户密码不会代替 App 专用密码访问邮件。

### 2. 同步已有隐私邮箱目录

App 专用密码只开放 IMAP 邮件访问，不能读取 Apple 账户中已有的完整“隐藏邮件地址”目录。要自动导入该目录，请在主号详情页点击“同步隐私邮箱”，首次连接时填写 Apple ID、Apple 账户密码，并按提示输入 6 位双重认证验证码。Apple 账户密码和验证码只用于建立受信任的 Apple Web 会话，不写入数据库；服务仅使用主密钥加密保存会话材料。会话过期后需要重新登录。

目录同步读取 Apple `premiummailsettings` 服务的 `/v2/hme/list` 完整列表，不会扫描收件箱，也不会只导入曾经收到过邮件的地址。服务会保留列表中的启用和停用地址，并只导入 `forwardToEmail` 与当前主号邮箱匹配的条目；不匹配的条目会被过滤。超过本地启用容量的地址仍会导入，但初始标记为 `disabled`。每个新导入地址都会生成只显示一次的独立邮件 API Key，已有地址保持原 Key、本地启停状态和邮件快照。

### 3. 确认隐私邮箱归属

本服务不会通过 Apple 接口创建“隐藏邮件地址”。请先在 Apple/iCloud 中创建并启用隐私邮箱，确认其邮件转发到准备登记的主号，然后在本服务后台或通过外部登记接口逐个登记这些地址。一个隐私邮箱只能登记一次，也只能归属于一个主号。

## 使用 Docker Compose 启动

Compose 内建 `icloud-api` 和 PostgreSQL 17 两个服务。根 Dockerfile 会先用 Node 构建 Vue 管理端，再编译 Go 服务，并把两者一起放入应用镜像；运行时由一个 Go 进程、一个 HTTP 端口同时提供 Vue 静态资源、JSON API 和 IMAP 同步。应用容器使用非 root 用户和只读根文件系统。

PostgreSQL 不发布宿主机端口，并设置 `network_mode: none` 和空的 `listen_addresses`。数据库与应用只通过命名卷 `postgres_socket` 中的 Unix Socket 通信；应用侧以只读方式挂载这个 Socket 卷。首次启动会随机生成数据库名、数据库用户、64 位十六进制数据库密码和安装标识，使用本地 `SCRAM-SHA-256` 认证，并通过 `postgres_config` 让应用入口自动组装连接 URL。数据库参数不会写入 `.env`，也不会开放 PostgreSQL TCP 服务。

Compose 使用以下命名卷：

- `postgres_data`：当前全部运行数据，包括主号、隐私邮箱、邮件快照、同步游标、会话和审计记录。
- `postgres_socket`：两个容器之间的 Unix Socket，仅用于通信，不需要备份。
- `postgres_config`：随机数据库凭据和安装标识；应用只读挂载。
- `installation_state`：不含密钥的数据库集群/主密钥绑定标记，用于发现卷丢失或不同安装的卷被混用。
- `icloud_api_keys`：自动生成的主密钥、管理员密码和 OAuth Token；显式使用环境变量覆盖的凭据不会写入这里。
- `icloud_api_data`：从旧版本保留的 SQLite 卷，只读挂载为自动迁移来源，不再承载当前运行数据。

新部署无需创建 `.env`，直接构建并启动即可：

```bash
docker compose up -d --build --wait
docker compose ps
APP_ADDR="$(docker compose port icloud-api 8080)"
curl -fsS "http://${APP_ADDR}/healthz"
```

默认管理员用户名是 `admin`。全新部署会自动生成管理员密码和 OAuth Token，分别以 `0600` 权限保存到 `/app/keys/admin-password` 和 `/app/keys/oauth-token`，不会把完整值写入应用日志。通过以下命令从持久化文件查看自动生成值：

```bash
MSYS_NO_PATHCONV=1 docker compose exec -T icloud-api cat /app/keys/admin-password
MSYS_NO_PATHCONV=1 docker compose exec -T icloud-api cat /app/keys/oauth-token
```

上述文件包含敏感凭据，应限制访问。若使用集中式 Secret 管理，也可以创建 `.env` 覆盖应用凭据和其他可选参数；只填写需要覆盖的项目：

```dotenv
ICLOUD_API_ADMIN_USER=admin
ICLOUD_API_ADMIN_PASSWORD=请替换为至少12位的高强度密码
ICLOUD_API_OAUTH_TOKEN=请替换为32至4096个无空白字符的随机令牌
ICLOUD_API_MASTER_KEY=请粘贴32字节Base64或十六进制主密钥
ICLOUD_API_PORT=8080
ICLOUD_API_COOKIE_SECURE=false
# 单个主号一整轮同步的总时限，至少为单次 IMAP 操作超时的两倍
ICLOUD_API_SYNC_TIMEOUT=2m
TZ=Asia/Shanghai
```

从旧版本升级时，如果现有 `.env` 曾把 `ICLOUD_API_SYNC_TIMEOUT` 设为 `10s`，请在重新构建或重启容器前先改为 `2m`。新版本会拒绝启动同步总时限不足单次 IMAP 操作时限两倍的配置，避免服务启动后所有同步都稳定超时。

留空时，Compose 会生成 48 位十六进制管理员密码、64 位十六进制 OAuth Token 和 32 字节主密钥。通过环境变量覆盖时，入口脚本直接使用提供值，不会同步写入 `icloud_api_keys`；重启和恢复时必须继续从 Secret 管理系统注入相同值。OAuth Token 不要与管理员密码、主密钥或邮件 API Key 复用。

`.env` 若存在，会包含登录凭据、OAuth Token 和主密钥，不要提交到版本库，也不要放入镜像。生产环境优先通过部署平台的 Secret 机制注入这些值。若经由 HTTPS 反向代理对外提供后台，请把 `ICLOUD_API_COOKIE_SECURE` 设为 `true`。

Compose 会先等待 PostgreSQL 健康，再启动应用。首次启动由应用自动创建 PostgreSQL 表和索引；后续启动会在事务中检查并执行数据库结构迁移。

早期 PostgreSQL 镜像在首次初始化时可能把仅用于绑定 PGDATA 卷的“安装标识 + 设备号/inode”复合值误写为安装标识，表现为 PostgreSQL 反复报告“安装标识格式错误”。更新后的数据库入口会在启动前识别并修复这一种精确状态：`postgres_config` 中必须仍有合法的 32 位安装标识，PGDATA 中的前三行凭据必须完全相同，第 4 行必须精确等于该标识与当前 PGDATA 目录身份的绑定值，两个数据库完成标记和应用主密钥状态也必须相互印证。修复使用可重入的分阶段原子写入；任何字段、权限、链接或卷身份不匹配时仍会停止启动，不会截断未知值或猜测卷归属。升级时保留全部命名卷并使用 `docker compose up -d --build --wait` 重建镜像；不要使用 `docker compose down -v`。

### 从 SQLite 旧版本升级

升级时，原命名卷 `icloud_api_data` 会只读挂载到 `/app/legacy`。如果其中存在 `/app/legacy/icloud-api.db`，且新的 PostgreSQL 业务表仍为空，应用会在启动阶段自动把管理员、后台会话、主号、隐私邮箱、最新邮件快照、审计记录和 Apple Web 会话复制到 PostgreSQL。导入使用事务、数据库级互斥锁和完成标记；成功后重复启动不会重复写入。旧 SQLite 文件不存在时会直接跳过。

旧版数据库采用 SQLite WAL 模式。迁移前必须停止所有仍可能写入旧库的应用和临时容器，确认没有其他读写挂载，并让 `icloud_api_data` 在整个迁移期间保持只读；程序还会在读取前后复核主文件及 sidecar 状态，发现变化就回滚导入。对于已干净关闭、且同时不存在 `icloud-api.db-wal`、`icloud-api.db-shm` 和 `icloud-api.db-journal` 的快照，程序会在不修改旧卷的前提下安全只读导入。只要任一 sidecar 存在，就必须将主文件和全部 sidecar 作为同一快照完整保留，并确保容器 UID `10001` 可读；WAL 快照的 `-wal` 和 `-shm` 必须同时存在，且不能混有 rollback journal。主文件和 sidecar 都必须是普通文件，不接受符号链接。

不得删除、截断或改名 sidecar，也不得使用 `immutable` 强制忽略它们，否则可能丢失尚未写回主文件的数据。如果保存的快照只有 `-wal`、只有 `-shm`，或同时存在 WAL 与 rollback journal，不要自行补建或清理文件；应回到同一恢复点取得完整快照后再迁移。

如果日志出现 `unable to open database file (14)`，先停止 `icloud-api` 应用，再核对旧卷是否挂载到 `/app/legacy`、目录和主文件的 UID/权限，以及所有 sidecar 的存在性、大小和可读性。在完成这些核对前，不要对旧卷或 sidecar 执行任何“清理”。

升级前保留旧 `.env` 中的 `ICLOUD_API_ADMIN_USER`、`ICLOUD_API_ADMIN_PASSWORD`、`ICLOUD_API_OAUTH_TOKEN` 和 `ICLOUD_API_MASTER_KEY`。管理员记录及密码哈希会从 SQLite 原样导入，修改环境变量不会覆盖已有管理员；检测到旧 SQLite 时，入口也不会生成一个实际无法登录旧管理员的假密码，仍应使用原管理员密码。OAuth Token 原本不在数据库中，改变它会让旧调用方立即失效；IMAP 凭据和 Apple Web 会话密文则必须使用原主密钥解密。

如果旧部署使用文件主密钥，并且 `/app/legacy/icloud-api.db.key` 与旧数据库同时存在，首次 PostgreSQL 引导会在没有显式 `ICLOUD_API_MASTER_KEY`、且 `icloud_api_keys` 中也没有 `master.key` 时复制旧密钥；已恢复的密钥文件和显式配置始终优先，不会被旧卷覆盖。如果检测到旧 SQLite 数据库但既没有该 `.key` 文件，也没有原环境变量主密钥，应用会停止启动，不会生成错误的新密钥。导入成功并完成登录、IMAP 同步和 API 验证前，保留 `icloud_api_data` 卷作为回退副本。

`ICLOUD_API_PORT` 是这个单一容器发布到宿主回环地址的端口。后台页面、`/admin/api/`、`/api/` 和 `/healthz` 都由同一个端口提供，因此 1Panel 只需反代到 `127.0.0.1:${ICLOUD_API_PORT}`，不需要按路径配置多个上游。这里的 `${ICLOUD_API_PORT}` 指 `.env` 中的实际值；上面的 `docker compose port` 命令可直接取得当前映射地址，避免把非默认端口误写成 `8080`。

1Panel 的反向代理必须保留浏览器访问时的外部主机名和协议。至少确认生成的 Nginx 配置包含以下请求头设置；非标准端口场景必须使用 `$http_host`，因为 `$host` 不保留端口。不要删除或改写浏览器发送的 `Origin`，否则后台登录和所有写操作会被同源 CSRF 校验拒绝为 `403`：

```nginx
proxy_set_header Host $http_host;
proxy_set_header Origin $http_origin;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
```

1Panel 从宿主机经回环发布端口访问容器时，应用看到的直接来源通常是 Docker bridge 网关。Compose 默认信任 RFC 1918 私有网段，以便从 `X-Forwarded-For` 恢复真实客户端 IP；因为端口只绑定 `127.0.0.1`，外部客户端不能直接绕过 1Panel 连接该端口。若已知实际网关地址或 bridge CIDR，建议通过 `ICLOUD_API_TRUSTED_PROXIES` 将范围收紧，然后重新创建容器。

Docker 构建默认使用大陆镜像源：基础镜像经由 DaoCloud（`docker.m.daocloud.io`）拉取，Go 模块使用七牛云 `Goproxy.cn`，npm 使用 npmmirror，Alpine 软件包使用阿里云镜像。Go 模块代理未配置海外直连回退，避免构建失败时绕过国内源；如需切换为企业内网镜像，可覆盖对应构建参数：

```bash
docker compose build \
  --build-arg DOCKER_HUB_MIRROR=registry.example.cn \
  --build-arg GOPROXY=https://goproxy.example.cn \
  --build-arg NPM_REGISTRY=https://npm.example.cn \
  --build-arg ALPINE_MIRROR=mirrors.example.cn
docker compose up -d
```

后台地址为 `http://127.0.0.1:${ICLOUD_API_PORT}/admin/`，其中端口取 `.env` 中的实际值（访问 `/admin` 会自动跳转）。查看启动日志：

```bash
docker compose logs -f icloud-api
```

前端请求和 API 请求都记录在同一个应用日志中。生产部署只运行一个 `icloud-api` 副本；当前主号同步互斥和接口限流保存在进程内，多副本会重复轮询 IMAP 并拆分限流状态。PostgreSQL 由 Compose 单独持久化，不改变这一应用单副本约束。

## 从源码启动

后端需要 Go 1.26 或与 `go.mod` 兼容的更新版本，前端需要 Node.js 22 或更新的 LTS 版本。首次启动前至少设置管理员密码：

```bash
export ICLOUD_API_ADMIN_USER=admin
export ICLOUD_API_ADMIN_PASSWORD='请替换为至少12位的高强度密码'
export ICLOUD_API_MASTER_KEY='请粘贴32字节Base64主密钥'
export ICLOUD_API_OAUTH_TOKEN='请替换为32至4096个无空白字符的随机令牌'
export ICLOUD_API_DATABASE_URL='postgres://icloud_api:数据库密码@127.0.0.1:5432/icloud_api?sslmode=disable'
export ICLOUD_API_ADDR=127.0.0.1:8080
go run ./cmd/icloud-api
```

源码运行需要先准备 PostgreSQL，并通过 `ICLOUD_API_DATABASE_URL` 提供连接 URL。Compose 部署已经自动提供该 URL，不要把上面的示例密码或宿主机地址加入 Compose 配置。

另开一个终端启动 Vue 开发服务器；开发代理会把管理 API、收件 API和健康检查转发给上面的 Go 服务：

```bash
cd web
npm ci
npm run dev
```

开发管理端默认位于 `http://127.0.0.1:5173/admin/`。如需在本地验证与 Docker 一致的单进程托管方式，先执行 `cd web && npm run build`，再从项目根目录设置 `ICLOUD_API_WEB_ROOT=web/dist` 后启动 Go 服务。生产环境使用前面的 Compose 构建，不需要在服务器上单独安装 Node.js，也不需要运行第二个 Web 服务。

源码直接运行时，首次初始化必须设置 `ICLOUD_API_ADMIN_PASSWORD`；Compose 入口脚本则会在留空时自动生成并持久化。管理员用户名和密码只在数据库中没有管理员的首次启动时用于初始化；已有数据库不会因为修改环境变量而修改现有登录凭据。

如果更换了 `.env` 中的管理员用户名或密码，已有数据卷中的管理员不会自动改密。为保留主号、隐私邮箱、邮件和审计数据，可在项目目录执行显式重置命令：

```bash
docker compose run --rm --no-deps icloud-api admin reset
```

命令使用当前 Compose 环境变量覆盖值，未覆盖时使用 `icloud_api_keys` 中自动生成的管理员密码，在 PostgreSQL 事务中更新管理员用户名和密码、递增凭据版本并注销旧后台会话，不会删除业务数据。入口只在 `admin reset` 子命令中依次取得 `installation_state/app-state/maintenance.lock` 和 keys 卷互斥锁，并持有到数据库事务及密码文件提交全部结束；并发的重置、备份或恢复任务会在修改数据前停止。正常应用进程不持有维护锁，因此常规重置无需先停止应用。旧版本升级后如果还没有 `/app/keys/admin-password`，命令会先把候选密码以 `0600` 权限保存到 `admin-password.pending`，数据库事务成功后再原子晋升为正式密码；中断后重新执行命令会复用同一个候选值。日志只提示保存路径和查看命令，不输出完整密码。校验或数据库更新失败时，不会删除原密码文件或修改密码来源标记。数据库中只有一个管理员且用户名已经更换时，命令会把该唯一管理员改名；存在多个管理员且当前用户名不存在时，命令会停止以避免选错账号。执行前建议按“备份与恢复”章节先做一次备份。

服务会自动创建或迁移 PostgreSQL 表结构。如果没有设置 `ICLOUD_API_MASTER_KEY`，应用会在 `ICLOUD_API_MASTER_KEY_FILE` 指定的位置生成本地主密钥文件；Compose 将该文件放在 `icloud_api_keys` 卷中。也可以先构建二进制：

```bash
go build -trimpath -o icloud-api ./cmd/icloud-api
./icloud-api
```

## 环境变量

Go 时长使用 `10s`、`1m`、`8h` 这类格式；正文大小使用字节数。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ICLOUD_API_ADDR` | `127.0.0.1:8080` | HTTP 监听地址；容器内设置为 `0.0.0.0:8080` |
| `ICLOUD_API_DATABASE_URL` | `postgres://icloud_api@/icloud_api?host=/var/run/postgresql&sslmode=disable` | 源码运行的 PostgreSQL 连接 URL，只接受 `postgres://` 或 `postgresql://`；Compose 入口会用 `postgres_config` 中的随机参数动态注入，不使用这个静态默认值 |
| `ICLOUD_API_LEGACY_SQLITE` | 空（Compose 为 `/app/legacy/icloud-api.db`） | 仅用于一次性自动导入的旧 SQLite 文件；当前业务不会写入该文件 |
| `ICLOUD_API_WEB_ROOT` | 空（镜像内为 `/app/web`） | Vue 生产构建目录；设置后 Go 会校验并提供其中的 `index.html` 和 `assets` |
| `ICLOUD_API_MASTER_KEY_FILE` | `data/master.key`（Compose 为 `/app/keys/master.key`） | 未设置环境变量主密钥时加载或自动生成的主密钥文件；Compose 通过 `icloud_api_keys` 卷持久化 |
| `ICLOUD_API_MASTER_KEY` | 空 | 32 字节 Base64 或十六进制主密钥；设置后优先于文件。Compose 留空时自动生成；SQLite 升级须沿用旧值，或保留可自动复制的旧 `.db.key` 文件 |
| `ICLOUD_API_ADMIN_USER` | `admin` | 首次初始化或显式 `admin reset` 时使用的管理员用户名 |
| `ICLOUD_API_ADMIN_PASSWORD` | 空 | 首次初始化或显式 `admin reset` 时使用的管理员密码，长度为 12 到 72 字节；Compose 留空时自动生成并保存到 `icloud_api_keys` |
| `ICLOUD_API_OAUTH_TOKEN` | 空 | 外部登记接口使用的 OAuth Bearer 令牌，长度为 32 至 4096 个字符且不得包含空白；Compose 留空时自动生成并保存到 `icloud_api_keys`，不得与其他凭据复用 |
| `ICLOUD_API_COOKIE_SECURE` | `false` | 是否只允许浏览器通过 HTTPS 发送后台会话 Cookie |
| `ICLOUD_API_SESSION_TTL` | `8h` | 后台登录会话有效期 |
| `ICLOUD_API_POLL_INTERVAL` | `1m` | IMAP 轮询间隔，范围 `10s` 到 `24h`；收件快照的新鲜度窗口为该值的三倍 |
| `ICLOUD_API_IMAP_TIMEOUT` | `25s` | 单次 IMAP 网络操作超时 |
| `ICLOUD_API_SYNC_TIMEOUT` | `2m` | 单个主号一整轮同步的总时限，范围 `10s` 到 `30m`，且至少为 `ICLOUD_API_IMAP_TIMEOUT` 的两倍；地址较多或网络较慢时应继续增大 |
| `ICLOUD_API_SYNC_CONCURRENCY` | `3` | 同时同步的主号数，范围 `1` 到 `16` |
| `ICLOUD_API_SHUTDOWN_TIMEOUT` | `10s` | 收到退出信号后的优雅关闭时限 |
| `ICLOUD_API_MAX_MESSAGE_BYTES` | `10485760` | 单封邮件最多读取的原始字节数，默认 10 MiB |
| `ICLOUD_API_MAX_BODY_BYTES` | `1048576` | 解析后正文总量上限，默认 1 MiB |
| `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS` | `false` | 是否在缺少投递头时允许用 `To`/`Cc` 归属邮件；详见安全事项 |
| `ICLOUD_API_TRUSTED_PROXIES` | 空（Compose 默认使用私有网段） | 逗号分隔的受信反向代理 IP/CIDR；Compose 端口仅发布到宿主回环地址，默认信任 Docker bridge 可能使用的 RFC 1918 地址，已知实际网关或 CIDR 时应收紧 |
| `GIN_MODE` | `release` | Gin 运行模式 |
| `TZ` | 系统时区 | `/api/v1/mail/recent` 的 `data.sent_at` 时区；须为 Go 可加载的 IANA、`UTC` 或 `Local` 时区名称，否则服务拒绝启动；紧凑接口按该时区返回带偏移的 RFC 3339，`/api/v1/mail/latest` 仍返回 RFC 3339/UTC；管理端 SPA 的时间显示取浏览器本地时区 |

主密钥也可以用内置命令生成：

```bash
go run ./cmd/icloud-api keygen
```

数据库中的 IMAP App 专用密码和受信任 Apple Web 会话使用彼此隔离的主密钥上下文进行 AES-GCM 加密；管理端可复制的邮件 API 直达链接凭据也由主密钥签名。Apple 账户密码和 6 位验证码不落库。主密钥一旦丢失，现有 IMAP 密文和 Apple 会话将失去解密条件，已经复制的派生直达链接也会失效。显式设置 `ICLOUD_API_MASTER_KEY` 时从 Secret 管理系统或受保护的 `.env` 单独备份；自动生成时应备份整个 `icloud_api_keys` 卷，其中还包含自动生成的管理员密码和 OAuth Token。应用密钥必须与对应 PostgreSQL 备份组成同一份恢复集。

## 后台操作

1. 打开 `/admin/login`，使用首次启动时配置的管理员账户登录。
2. 添加主号，填写名称、iCloud 主号邮箱、IMAP 用户名和 App 专用密码。
3. 进入主号详情页点击“同步隐私邮箱”。首次连接使用 Apple 账户密码和 6 位双重认证建立受信任会话，随后从 Apple 账户的完整目录批量导入该主号的隐私邮箱；也可以继续手动逐个登记。
4. 立即保存本次新导入地址的一次性 API Key；离开结果页后服务端无法恢复原始 Key。
5. 使用“同步邮件”检查 IMAP 连接和收件状态；后台也会按轮询间隔自动同步所有已启用主号。积压超过单批上限时，页面会提示“已提交一批，仍在追平”，后续轮询继续处理，而不会虚报全部完成。
6. 在主号详情或全部隐私邮箱页面查看归属、状态和最近收件时间，并从操作列复制邮件 API 直达链接。需要时可以停用、删除或轮换 Key。
7. 在操作记录页面检查登录、配置变更、目录同步、邮件同步和 Key 轮换记录。

主号列表、主号详情和隐私邮箱列表会在页面可见时每 5 秒静默读取最新同步状态与时间；切换到其他标签页时暂停，返回页面或窗口重新获得焦点时立即刷新。旧版回退管理页使用相同的刷新规则。

轮换 Key 会直接吊销旧 Key。Vue 管理端只会在创建或轮换 Key 的结果中显示一次完整 API Key；离开结果页后无法从数据库重建，因为数据库不保存原始 Key。未保存原始 Key 时需要再次轮换。

邮件 API 直达链接不需要保存原始 Key：服务端会为管理端生成一个仅用于紧凑链接接口的派生凭据，并在主号详情和全部隐私邮箱列表的操作列提供复制按钮。派生凭据不写入数据库，也不能用于 Bearer 接口；它与当前 API Key 状态绑定，轮换 Key 会同时吊销此前复制的直达链接。

## 外部登记接口

外部系统可通过 `POST /api/v1/aliases` 把一个**已在 Apple/iCloud 中创建并启用**的隐私邮箱登记到现有主号。本接口只登记地址，不会调用 Apple 创建隐私邮箱。请求头必须携带部署时配置的独立 OAuth 令牌：

```http
POST /api/v1/aliases
Authorization: Bearer <ICLOUD_API_OAUTH_TOKEN>
Content-Type: application/x-www-form-urlencoded

add_hide_my_eamil=alias%40icloud.com&icloud=primary%40icloud.com
```

字段名 `add_hide_my_eamil` 为外部兼容契约中的既有拼写，必须按此拼写发送；其值是待登记的隐私邮箱。`icloud` 是该隐私邮箱所属、且已在管理端登记的 iCloud 主号邮箱。隐私邮箱不能与任何已登记主号邮箱或所选主号的 IMAP 用户名相同。推荐把两个字段放在 `application/x-www-form-urlencoded` 正文中：

```bash
curl --fail-with-body \
  -X POST \
  -H "Authorization: Bearer ${ICLOUD_API_OAUTH_TOKEN}" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'add_hide_my_eamil=alias@icloud.com' \
  --data-urlencode 'icloud=primary@icloud.com' \
  https://mail.example.com/api/v1/aliases
```

为兼容已有调用方，也可把参数放在 URL 查询串中，例如 `POST /api/v1/aliases?add_hide_my_eamil=alias%40icloud.com&icloud=primary%40icloud.com`。查询串会暴露邮箱到代理日志、监控和浏览器历史，因此只建议作为兼容方式使用。每个字段在查询串与正文合计必须恰好出现一次；不要同时在两处传同一字段，也不要发送未知字段。只要请求带有正文，`Content-Type` 就必须是 `application/x-www-form-urlencoded`。

成功时返回 `201 Created`。`api_key` 是新隐私邮箱仅显示一次的原始邮件 API Key；`mail_api_direct_link` 是同源相对路径，查询参数使用服务端派生的直达凭据，而不是把原始 `api_key` 直接拼入 URL：

```json
{
  "data": {
    "alias": "alias@icloud.com",
    "icloud": "primary@icloud.com",
    "api_key": "icm_REPLACE_WITH_GENERATED_ALIAS_KEY",
    "mail_api_direct_link": "/api/v1/mail/recent?api_key=icm_REPLACE_WITH_DERIVED_TOKEN"
  }
}
```

新登记的地址初始同步状态为 `pending`，直达链接会在下一轮同步确认该地址状态后按收件 API 规则返回邮件；同步确认前会返回 `503 SYNC_UNAVAILABLE`。新增地址、批量导入至少一个启用地址或重新启用地址会使所属主号的增量游标失效；下一轮按首次同步规则一次性有界回扫收件箱中实际存在的最新最多 1024 封邮件，然后建立新的 `UIDVALIDITY + last_uid` 基线。

所有响应均包含 `Cache-Control: no-store`。接口可能返回 `400`（字段缺失、重复、未知、邮箱格式错误或地址与主号身份冲突）、`401`（OAuth Bearer 令牌缺失或无效）、`404`（主号不存在）、`409`（隐私邮箱已登记或主号已达地址上限）、`413`（URL 编码参数合计超过 4 KiB）、`415`（正文媒体类型错误）、`429`（请求过于频繁）、`500`（邮件 API Key 或直达凭据生成异常）或 `503`（数据库暂不可用）。响应中的 `api_key` 和直达链接都属于敏感凭据，应立即保存并只通过 HTTPS 传输。

## 收件 API

完整接口定义见 [`docs/openapi.yaml`](docs/openapi.yaml)。服务提供一个可直接访问的紧凑链接接口，以及一个使用 Bearer Header 的完整接口。两个接口都只读取当前最新快照：紧凑接口仅在其 IMAP 收件时间位于 `[当前时间-1小时, 当前时间]` 时返回 `200`；完整 Bearer 接口保留原有语义，只要同步状态仍新鲜就返回已保存的最新快照，不额外限制邮件年龄。

### 紧凑链接接口

```http
GET /api/v1/mail/recent?api_key=icm_<后台复制的43位RawBase64URL凭据>
```

该接口方便只能直接打开 URL 的调用方使用。`api_key` 必填且只能出现一次，只用于定位它绑定的唯一隐私邮箱；Bearer Header 不能替代这个查询参数，接口也不接受由调用方指定的邮箱地址。查询参数既兼容创建或轮换隐私邮箱时显示的完整 API Key，也接受管理端操作列复制的派生直达凭据；派生凭据不能反过来用于 Bearer 接口。以下占位值故意不是有效 Key，实际使用时可直接在后台复制完整链接：

```text
https://mail.example.com/api/v1/mail/recent?api_key=icm_REPLACE_WITH_ALIAS_KEY
```

成功响应是带 `data` 信封层的 JSON 对象：

```json
{
  "data": {
    "address": "user@icloud.com",
    "subject": "Your ChatGPT verification code",
    "snippet": "Your code is 123456",
    "sent_at": "2026-08-07T12:30:00+08:00"
  }
}
```

`address` 是 Key 绑定的隐私邮箱，`subject` 是邮件主题；`snippet` 返回单行纯文本，优先使用邮件声明的纯文本正文，纯文本为空时从 HTML 正文中提取可读文字，并过滤结构标签、样式、脚本及常见的内联隐藏内容。两种来源中的换行、制表符和连续空白都会压缩成一个普通空格。内容可能因服务端邮件或正文大小上限而截断；`sent_at` 优先使用邮件 `Date` 头，缺失或无效时回退到 IMAP 服务器记录的收件时间，并按 `.env` 中 `TZ` 对应的时区格式化为 RFC 3339。上例使用 `TZ=Asia/Shanghai`，因此偏移为 `+08:00`。时区只改变显示形式，不改变按 IMAP 收件时间计算的“最近一小时”绝对时间窗口。

查询参数会进入浏览器历史，并可能被反向代理或外围监控记录、通过 Referer 泄露。本应用内置 HTTP 日志只记录 URL 路径，不记录查询字符串，但不能代替外围系统的脱敏配置。完整链接必须和 API Key 一样按密钥保护。能设置请求头的客户端应优先使用下面的 Bearer 接口；确实使用紧凑链接时，只能通过 HTTPS 传输，并应在代理层关闭或脱敏查询字符串日志，不要把链接发到工单、聊天、分析平台或第三方页面。怀疑链接泄露后立即在后台轮换 Key。

### 完整 Bearer 接口

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

`id` 由 IMAP `UIDVALIDITY` 和 `UID` 组成，可以用于判断两次读取是否仍为同一封邮件。`sent_at` 来自邮件头，缺失时为 `null`；`received_at` 来自 IMAP 服务器。完整接口中的 `received_at`、`sent_at` 和 `synced_at` 继续统一使用 RFC 3339/UTC，不受 `TZ` 的显示设置影响。`body_truncated=true` 表示邮件或正文超过配置上限，返回内容可能不完整。附件只返回文件名、类型和大小元数据，不返回附件内容。

常见状态码：

| 状态码 | 错误码 | 含义 |
| --- | --- | --- |
| `401` | `INVALID_API_KEY` | Bearer Header 或 `api_key` 查询参数缺失，Key 不符合 `icm_` 加 43 位规范 Raw Base64URL、内容未知，或者对应主号/隐私邮箱已停用 |
| `404` | `MAIL_NOT_FOUND` | 同步结果仍新鲜但没有邮件快照；紧凑接口还会在最新快照的 IMAP 收件时间不处于 `[当前时间-1小时, 当前时间]` 时返回此错误，完整 Bearer 接口不限制邮件年龄 |
| `429` | `RATE_LIMITED` | 单个 Key 请求过于频繁；当前进程内限制为每分钟 120 次 |
| `503` | `SYNC_UNAVAILABLE` | 该隐私邮箱为 `pending`/`error`、从未完成同步、最后同步时间晚于当前时间，或结果已超过三个轮询周期 |
| `503` | `DATABASE_UNAVAILABLE` | 查询 Key 或最新邮件快照时数据库暂不可用 |

错误响应统一包含错误码、说明和请求编号。下面是紧凑接口在当前快照不属于最近一小时窗口时的示例；完整 Bearer 接口没有邮件年龄限制，且无快照时使用“尚未收到邮件”：

```json
{
  "error": {
    "code": "MAIL_NOT_FOUND",
    "message": "最近一小时内没有邮件",
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

- 收件 API 只查询 PostgreSQL 中的最新邮件快照，不会在每次 API 调用时连接 iCloud；因此延迟通常为一个 `ICLOUD_API_POLL_INTERVAL`，外加 IMAP 网络耗时。
- 邮件正文、文件夹和已读/收件状态始终由 `imap.mail.me.com:993` 的只读 IMAP 连接获取；Apple Web 会话只用于读取隐私邮箱完整目录，不参与邮件收取。
- 每轮邮件同步按主号建立一个只读 IMAP 连接，并处理这个主号下的所有已启用隐私邮箱，不会为每个隐私邮箱分别查询 iCloud。常规增量的 UID 数值跨度不超过 1024 时，只执行一次主号级 `UID SEARCH` 来发现新邮件；跨度更大或 UID 很稀疏时，改用响应有界的 sequence-to-UID 探测和一批最多 1024 封邮件，不会重复枚举全部剩余 UID。候选邮件头按 UID 批量读取并在本地归类，只对各地址最终命中的最新候选读取完整正文。单封邮件的投递头损坏、超限或相互冲突时，只跳过该封不可安全归属的邮件，不会阻塞整个主号的游标；IMAP 命令失败、重复 UID 等协议异常仍会让本轮失败并重试。
- PostgreSQL 为每个主号保存 `UIDVALIDITY + last_uid`，并保存每个隐私邮箱当前快照的 UID 位置。`UIDVALIDITY` 未变化时从 `last_uid + 1` 开始处理；追平新增邮件后，服务用一条共享 `UID FETCH` 校验所有已保存的最新 UID。若邮件已从 iCloud 删除，服务会共享扫描最近窗口，为受影响地址回退到窗口内次新邮件；没有可用回退时删除本地快照。整个过程仍是主号级批量操作，不会产生逐隐私邮箱查询。
- 首次同步或 `UIDVALIDITY` 变化时会建立新游标、重置旧快照，并且只扫描收件箱中实际存在的最新最多 1024 封邮件，不会全量下载历史邮件。UID 即使因删除邮件而不连续，也不会因此缩小实际邮件窗口。
- 正常增量积压超过 1024 封时，每轮从最早尚未处理的实际邮件开始处理最多 1024 封，游标停在本批末尾并在后续轮次继续追平，不会跨过旧邮件。追平期间主号及其启用隐私邮箱保持 `pending`，收件 API 返回 `503`；全部追平后才标记为 `ok`。
- 新建隐私邮箱、批量导入至少一个启用项、重新启用隐私邮箱、更新主号 IMAP 凭据、重新启用主号或执行整主号快照重置，都会使主号游标失效；下一轮有界回扫实际存在的最新最多 1024 封邮件。同一 `UIDVALIDITY` 下，窗口外的已有快照经共享 UID 校验后保留；邮箱代际真正变化时才清理旧代快照。
- 邮件快照、主号游标和同步状态在同一个 PostgreSQL 事务中提交；任一步失败都不会提前推进游标。一次主号同步失败会把该主号及其已启用地址标为 `error`，其他主号不受影响；积压批次的成功提交仍保持 `pending`，不属于同步失败。
- 服务从 iCloud 转发邮件中的候选投递收件头精确解析邮箱地址，不做字符串包含匹配；这些普通邮件头的可信程度取决于下文所述的 iCloud 转发行为。单个主号最多启用并处理 1000 个隐私邮箱；完整目录中超出此容量的新地址仍会导入，但标记为 `disabled`。
- Apple 的 Hide My Email 转发邮件会额外带有 `X-ICLOUD-HME` 路由头（例如 `p=<隐私号>; f=<主号>; r=to`）。同步会单独解析并校验该头：`p` 必须出现在对应的 `To`/`Cc`，`f` 必须等于当前主号，且存在的 `Original-Recipient` 也必须与 `f` 一致；只有校验通过后才会按 `p` 归属隐私邮箱。`Original-Recipient` 指向主号是 Apple 的正常物理投递结果，不会覆盖 `p`。
- PostgreSQL 对每个隐私邮箱只保留一行最新邮件。新 UID 会替换旧 UID；当前最新邮件被删除时，只会在上述有界最近窗口内寻找回退，不会把 PostgreSQL 变成邮件归档库。接口也不会为了凑足一小时窗口而回退到更旧邮件。
- `pending` 表示地址刚创建、重新启用、因主号凭据变化而重置，或主号仍在分批追平积压；收件 API 返回 `503`。后台手动同步在成功提交一批但仍有积压时返回 HTTP `202`，页面明确显示“已提交一批，仍在追平”。
- `error` 表示最近同步没有确认该地址的最新状态。旧快照可能仍保存在内部，但 API 会返回 `503`，不会把它作为可用邮件返回。
- `ok` 表示最近同步已明确找到最新邮件，或权威确认当前为空。在三个轮询周期的有效期内，完整 Bearer 接口有快照就返回 `200`、没有快照时返回 `404`；紧凑接口仅在最新快照的 IMAP 收件时间处于 `[当前时间-1小时, 当前时间]` 时返回 `200`，没有快照或快照不在该窗口时返回 `404`。最后同步时间晚于当前时间或结果超过有效期时，两个接口都返回 `503`。
- 当前版本的成功响应中 `stale` 始终为 `false`。降级状态统一返回 `503`，不会返回带 `stale=true` 的旧快照。
- 使用完整接口的客户端可保存 `message.id` 并定时重试；ID 未变化时不要重复处理。紧凑接口不返回 ID，调用方需要自行按 `address`、`sent_at` 和 `snippet` 去重。建议轮询频率低于服务端每分钟 120 次的 Key 限流。

## 备份与恢复

当前业务数据位于 `postgres_data`。为保证 PostgreSQL 与 `icloud_api_keys` 属于同一恢复点，逻辑备份也应在维护窗口中执行：先取得固定名维护锁，停止应用并保持 PostgreSQL 运行，在同一个临时目录中生成并校验两份归档，再以一个版本化目录发布恢复集；应用通过健康检查后才释放维护锁。镜像内置的数据库包装会从 `postgres_config` 动态读取随机数据库名、用户和密码，在容器内部完成 SCRAM 认证；数据库端口无需发布到宿主机。任一备份、校验、发布或重新启动步骤失败时，下面的命令都会保持应用停止：

```bash
set -eu
umask 077

mkdir -p backup
backup_name="icloud-api-$(date -u +%Y%m%dT%H%M%SZ)-$$"
backup_tmp="$(mktemp -d "./backup/.${backup_name}.XXXXXX")" || exit 1
backup_final="./backup/${backup_name}"
maintenance_lock_started=false
maintenance_lock_container_id=""

maintenance_lock_is_ready() {
  [ "$maintenance_lock_started" = true ] || return 1
  [ -n "$maintenance_lock_container_id" ] || return 1
  if ! container_running="$(docker inspect --format '{{.State.Running}}' \
    "$maintenance_lock_container_id" 2>/dev/null)"; then
    return 1
  fi
  [ "$container_running" = true ] || return 1
  if ! ready_token="$(MSYS_NO_PATHCONV=1 docker exec \
    "$maintenance_lock_container_id" \
    cat /run/icloud-api-installation/app-state/maintenance-window.ready \
    2>/dev/null)"; then
    return 1
  fi
  [ "$ready_token" = "$maintenance_token" ]
}

require_maintenance_lock() {
  if ! maintenance_lock_is_ready; then
    echo "维护锁已丢失；丢弃未发布输出并保持应用停止。" >&2
    return 1
  fi
}

maintenance_lock_confirm_absent() {
  if ! maintenance_lock_present="$(docker container ls -a --no-trunc \
    --format '{{.ID}}' --filter "id=$maintenance_lock_container_id")"; then
    return 1
  fi
  [ -z "$maintenance_lock_present" ]
}

release_maintenance_lock() {
  [ "$maintenance_lock_started" = true ] || return 0
  [ -n "$maintenance_lock_container_id" ] || return 1
  if container_running="$(docker inspect --format '{{.State.Running}}' \
    "$maintenance_lock_container_id" 2>/dev/null)"; then
    case "$container_running" in
      true)
        docker stop -t 10 "$maintenance_lock_container_id" \
          >/dev/null 2>&1 || return 1
        ;;
      false) ;;
      *) return 1 ;;
    esac
  elif ! maintenance_lock_confirm_absent; then
    return 1
  fi
  if docker inspect "$maintenance_lock_container_id" >/dev/null 2>&1; then
    docker rm -f "$maintenance_lock_container_id" \
      >/dev/null 2>&1 || return 1
  elif ! maintenance_lock_confirm_absent; then
    return 1
  fi
  maintenance_lock_started=false
  maintenance_lock_container_id=""
}

cleanup_backup() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  if [ -n "${backup_tmp:-}" ] && [ -d "$backup_tmp" ]; then
    if ! rm -rf -- "$backup_tmp"; then
      echo "删除未发布备份失败：$backup_tmp" >&2
      status=1
    fi
  fi
  if [ "$status" -ne 0 ] && [ "$maintenance_lock_started" = true ]; then
    docker compose stop icloud-api >/dev/null 2>&1 || true
    echo "备份未完整成功，应用保持停止。" >&2
  fi
  if ! release_maintenance_lock; then
    echo "维护锁容器清理失败；应用保持停止，请检查 icloud-api-maintenance-lock。" >&2
    status=1
  fi
  exit "$status"
}
trap cleanup_backup EXIT
trap 'exit 1' HUP INT TERM

maintenance_token="$(od -An -N 16 -tx1 /dev/urandom | tr -d ' \n')"
if [ "${#maintenance_token}" -ne 32 ]; then
  echo "生成维护窗口就绪令牌失败；未修改应用状态。" >&2
  exit 1
fi
if ! maintenance_lock_container_id="$(docker compose run --rm --no-deps -d -T \
  --user 0:0 \
  --name icloud-api-maintenance-lock \
  postgres maintenance-lock "$maintenance_token")"; then
  echo "另一个管理员重置、备份或恢复任务正在运行；未修改应用状态。" >&2
  exit 1
fi
maintenance_lock_container_id="$(printf '%s' "$maintenance_lock_container_id" | tr -d '\r\n')"
[ -n "$maintenance_lock_container_id" ] || {
  echo "维护锁容器未返回容器 ID；未修改应用状态。" >&2
  exit 1
}
maintenance_lock_started=true
if ! maintenance_lock_container_id_full="$(docker inspect --format '{{.Id}}' \
  "$maintenance_lock_container_id" 2>/dev/null)"; then
  echo "读取维护锁容器 ID 失败；未修改应用状态。" >&2
  exit 1
fi
case "$maintenance_lock_container_id_full" in
  *[!0-9a-f]*|'')
    echo "维护锁容器 ID 格式错误；未修改应用状态。" >&2
    exit 1
    ;;
esac
if [ "${#maintenance_lock_container_id_full}" -ne 64 ]; then
  echo "维护锁容器 ID 长度错误；未修改应用状态。" >&2
  exit 1
fi
maintenance_lock_container_id="$maintenance_lock_container_id_full"
maintenance_ready=false
maintenance_attempt=0
while [ "$maintenance_attempt" -lt 100 ]; do
  maintenance_attempt=$((maintenance_attempt + 1))
  if maintenance_lock_is_ready; then
    maintenance_ready=true
    break
  fi
  if ! container_running="$(docker inspect --format '{{.State.Running}}' \
    "$maintenance_lock_container_id" 2>/dev/null)" || \
    [ "$container_running" != true ]; then
    break
  fi
  sleep 0.1
done
if [ "$maintenance_ready" != true ]; then
  release_maintenance_lock || true
  echo "维护锁容器未确认取得独占锁；未修改应用状态。" >&2
  exit 1
fi

require_maintenance_lock || exit 1
if ! docker compose stop icloud-api; then
  echo "停止应用失败。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
if ! docker compose up -d --wait postgres; then
  echo "PostgreSQL 未就绪。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
if ! MSYS_NO_PATHCONV=1 docker compose exec -T postgres \
  /usr/local/bin/icloud-api-postgres-entrypoint backup \
  > "$backup_tmp/icloud-api.dump"; then
  echo "PostgreSQL 备份失败。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
dump_magic="$(od -An -N 5 -tx1 "$backup_tmp/icloud-api.dump" | tr -d ' \n')"
if [ ! -s "$backup_tmp/icloud-api.dump" ] || [ "$dump_magic" != "5047444d50" ]; then
  echo "PostgreSQL 备份不是有效的 custom archive。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
if ! MSYS_NO_PATHCONV=1 docker compose run --rm --no-deps -T \
  --name icloud-api-keys-maintenance \
  --entrypoint /usr/local/bin/icloud-api-keys-maintenance \
  icloud-api backup > "$backup_tmp/icloud-api-keys.tar"; then
  echo "应用密钥备份失败。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
if [ ! -s "$backup_tmp/icloud-api-keys.tar" ] || \
  ! tar -tf "$backup_tmp/icloud-api-keys.tar" >/dev/null; then
  echo "应用密钥备份不是有效的 tar 归档。" >&2
  exit 1
fi
if ! chmod 600 \
  "$backup_tmp/icloud-api.dump" \
  "$backup_tmp/icloud-api-keys.tar"; then
  echo "设置备份权限失败。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
if [ -e "$backup_final" ] || ! mv "$backup_tmp" "$backup_final"; then
  echo "发布恢复集失败。" >&2
  exit 1
fi
backup_tmp=""
require_maintenance_lock || exit 1
if ! docker compose up -d --wait icloud-api; then
  echo "恢复集已发布，但应用重新启动失败。" >&2
  exit 1
fi
require_maintenance_lock || exit 1
if ! release_maintenance_lock; then
  echo "释放维护锁失败。" >&2
  exit 1
fi

trap - EXIT HUP INT TERM
printf '恢复集已保存到：%s\n' "$backup_final"
```

脚本中的 `umask 077` 和 `chmod 600` 会在 Linux 或 WSL 的 Linux 文件系统上收紧 Unix 权限。Windows Git Bash 把恢复集写到 NTFS 时，`chmod` 即使返回成功也不代表 Windows ACL 已收紧；建议把敏感备份保存在 WSL/Linux 文件系统中，或在项目目录的 PowerShell 中把下面的路径替换为刚生成的恢复集，再移除继承、授予当前用户和 SYSTEM，并确认输出中没有其他可读主体：

```powershell
$backupSet = (Resolve-Path '.\backup\icloud-api-20260808T120000Z-12345').Path
$currentUser = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
icacls $backupSet /inheritance:r /grant:r "${currentUser}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F'
icacls $backupSet
```

恢复集路径只由宿主 shell 用于完整性检查和输入重定向，不会作为 bind mount 传给 Docker。即使 Linux 宿主备份目录是 `0700`、归档是 `0600`，也由当前宿主用户打开文件并通过 stdin 把字节送进非 root 维护容器。所有携带容器绝对路径的 Docker 命令仍设置 `MSYS_NO_PATHCONV=1`，避免 Windows Git Bash 改写容器入口路径；该环境变量在 Linux 上没有额外作用。

`icloud-api-maintenance-lock` 容器由 PostgreSQL 镜像内置的 `maintenance-lock` 子命令启动；`--rm` 会覆盖服务的重启策略，锁获取失败的容器不会反复重启。该子命令可以在全新的 `installation_state` 卷中以正确所有者和权限初始化 `app-state`，随后在 `installation_state/app-state/maintenance.lock` 上持有独占 `flock`，覆盖“停应用、生成或恢复两份归档、重新启动并通过健康检查”的完整生命周期。它还同时持有 `maintenance-window.lock`，只用于证明调用者确实处于完整备份/恢复窗口，避免把一个短暂的普通 `admin reset` 误认成恢复上下文。两把锁都取得后，容器才原子发布本次随机 token；宿主同时确认 token 和容器运行状态后才停止应用，因此“容器已启动但尚未取得锁”不会被当作就绪。固定容器名只用于启动时的防重保护；脚本会保存 `docker compose run -d` 返回的本次容器 ID，后续 readiness、`exec`、`inspect`、`stop` 和 `rm` 全部按该 ID 操作，避免容器自动移除后同名新任务被误清理。任何 ID/令牌复核或 Docker 守护进程操作失败都会丢弃尚未发布的输出并保持应用停止。备份、恢复和独立 `admin reset` 使用同一个主锁文件，因此即使换用其他容器名也不会并发修改数据库与密钥。两个锁文件会留在卷中，互斥状态由存活进程持有，空闲时文件存在不表示仍被锁定。固定容器名 `icloud-api-keys-maintenance` 另用于密钥操作，维护脚本还会直接锁定 keys 卷目录；统一顺序是先取得宿主维护锁，再取得 keys 锁。`-T` 保证输出的 tar 字节不会经过 TTY。维护脚本只归档六种正式文件，不会把临时目录、未知文件或软链接带入恢复集。如果某项凭据通过 `.env` 或 Secret 管理，归档会保存对应的来源标记，还必须从密钥管理系统单独备份同一时刻的原值。

正常失败及 `HUP`、`INT`、`TERM` 会先保持应用停止，再清理维护锁。宿主维护终端被强制终止或 Docker daemon 中断时，`icloud-api-maintenance-lock` 可能继续存在并安全阻止下一次维护。先确认没有仍在执行的备份或恢复终端，并检查应用当前保持停止，再执行 `docker rm -f icloud-api-maintenance-lock`；随后必须从所选备份或恢复命令的第一行完整重跑，不要从数据库或密钥中间步骤继续。

`postgres_socket` 只含运行时 Socket，无需备份。SQLite 旧卷 `icloud_api_data` 在升级验收完成前应单独保留。若使用存储平台的物理卷快照，必须先停止应用和 PostgreSQL，再把 `postgres_data`、`postgres_config`、`installation_state` 和 `icloud_api_keys` 作为同一个恢复点处理，不能混用不同安装的卷；上面的逻辑备份只停止应用，PostgreSQL 保持运行，并且可以恢复到入口脚本新生成的随机数据库参数中。

恢复前，先从备份命令输出的版本化目录中选择一个完整恢复集，并恢复该恢复集配套的 `.env` 或 Secret。`RESET_EMPTY_POSTGRES=false` 适用于目标 PostgreSQL 数据卷仍然健康的覆盖恢复；只有 `postgres_data` 已丢失、为空或首次 `initdb` 被明确判定为未完成时，才把它设为 `true`。应用停止后会先完整预检密钥归档及其环境凭据，再执行任何 PostgreSQL 重置或恢复；数据库恢复、密钥恢复和待提交管理员密码任一步失败时，应用都会保持停止：

```bash
set -eu
BACKUP_SET=./backup/icloud-api-20260808T120000Z-12345
RESET_EMPTY_POSTGRES=false
maintenance_lock_started=false
maintenance_lock_container_id=""

maintenance_lock_is_ready() {
  [ "$maintenance_lock_started" = true ] || return 1
  [ -n "$maintenance_lock_container_id" ] || return 1
  if ! container_running="$(docker inspect --format '{{.State.Running}}' \
    "$maintenance_lock_container_id" 2>/dev/null)"; then
    return 1
  fi
  [ "$container_running" = true ] || return 1
  if ! ready_token="$(MSYS_NO_PATHCONV=1 docker exec \
    "$maintenance_lock_container_id" \
    cat /run/icloud-api-installation/app-state/maintenance-window.ready \
    2>/dev/null)"; then
    return 1
  fi
  [ "$ready_token" = "$maintenance_token" ]
}

require_maintenance_lock() {
  if ! maintenance_lock_is_ready; then
    echo "维护锁已丢失；丢弃未发布输出并保持应用停止。" >&2
    return 1
  fi
}

maintenance_lock_confirm_absent() {
  if ! maintenance_lock_present="$(docker container ls -a --no-trunc \
    --format '{{.ID}}' --filter "id=$maintenance_lock_container_id")"; then
    return 1
  fi
  [ -z "$maintenance_lock_present" ]
}

release_maintenance_lock() {
  [ "$maintenance_lock_started" = true ] || return 0
  [ -n "$maintenance_lock_container_id" ] || return 1
  if container_running="$(docker inspect --format '{{.State.Running}}' \
    "$maintenance_lock_container_id" 2>/dev/null)"; then
    case "$container_running" in
      true)
        docker stop -t 10 "$maintenance_lock_container_id" \
          >/dev/null 2>&1 || return 1
        ;;
      false) ;;
      *) return 1 ;;
    esac
  elif ! maintenance_lock_confirm_absent; then
    return 1
  fi
  if docker inspect "$maintenance_lock_container_id" >/dev/null 2>&1; then
    docker rm -f "$maintenance_lock_container_id" \
      >/dev/null 2>&1 || return 1
  elif ! maintenance_lock_confirm_absent; then
    return 1
  fi
  maintenance_lock_started=false
  maintenance_lock_container_id=""
}

cleanup_restore() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  if [ "$status" -ne 0 ] && [ "$maintenance_lock_started" = true ]; then
    docker compose stop icloud-api >/dev/null 2>&1 || true
    echo "恢复未完整成功，应用保持停止。" >&2
  fi
  if ! release_maintenance_lock; then
    echo "维护锁容器清理失败；应用保持停止，请检查 icloud-api-maintenance-lock。" >&2
    status=1
  fi
  exit "$status"
}
trap cleanup_restore EXIT
trap 'exit 1' HUP INT TERM

restore_failed() {
  exit 1
}

if [ ! -s "$BACKUP_SET/icloud-api.dump" ] || \
  [ ! -s "$BACKUP_SET/icloud-api-keys.tar" ]; then
  echo "恢复集不完整：$BACKUP_SET" >&2
  restore_failed
fi
maintenance_token="$(od -An -N 16 -tx1 /dev/urandom | tr -d ' \n')"
if [ "${#maintenance_token}" -ne 32 ]; then
  echo "生成维护窗口就绪令牌失败；未修改应用状态。" >&2
  exit 1
fi
if ! maintenance_lock_container_id="$(docker compose run --rm --no-deps -d -T \
  --user 0:0 \
  --name icloud-api-maintenance-lock \
  postgres maintenance-lock "$maintenance_token")"; then
  echo "另一个管理员重置、备份或恢复任务正在运行；未修改应用状态。" >&2
  exit 1
fi
maintenance_lock_container_id="$(printf '%s' "$maintenance_lock_container_id" | tr -d '\r\n')"
[ -n "$maintenance_lock_container_id" ] || {
  echo "维护锁容器未返回容器 ID；未修改应用状态。" >&2
  exit 1
}
maintenance_lock_started=true
if ! maintenance_lock_container_id_full="$(docker inspect --format '{{.Id}}' \
  "$maintenance_lock_container_id" 2>/dev/null)"; then
  echo "读取维护锁容器 ID 失败；未修改应用状态。" >&2
  exit 1
fi
case "$maintenance_lock_container_id_full" in
  *[!0-9a-f]*|'')
    echo "维护锁容器 ID 格式错误；未修改应用状态。" >&2
    exit 1
    ;;
esac
if [ "${#maintenance_lock_container_id_full}" -ne 64 ]; then
  echo "维护锁容器 ID 长度错误；未修改应用状态。" >&2
  exit 1
fi
maintenance_lock_container_id="$maintenance_lock_container_id_full"
maintenance_ready=false
maintenance_attempt=0
while [ "$maintenance_attempt" -lt 100 ]; do
  maintenance_attempt=$((maintenance_attempt + 1))
  if maintenance_lock_is_ready; then
    maintenance_ready=true
    break
  fi
  if ! container_running="$(docker inspect --format '{{.State.Running}}' \
    "$maintenance_lock_container_id" 2>/dev/null)" || \
    [ "$container_running" != true ]; then
    break
  fi
  sleep 0.1
done
if [ "$maintenance_ready" != true ]; then
  release_maintenance_lock || true
  echo "维护锁容器未确认取得独占锁；未修改应用状态。" >&2
  exit 1
fi

require_maintenance_lock || restore_failed
if [ "$RESET_EMPTY_POSTGRES" = true ]; then
  if ! docker compose stop icloud-api postgres; then
    echo "停止应用和 PostgreSQL 失败。" >&2
    restore_failed
  fi
elif [ "$RESET_EMPTY_POSTGRES" = false ]; then
  if ! docker compose stop icloud-api; then
    echo "停止应用失败。" >&2
    restore_failed
  fi
else
  echo "RESET_EMPTY_POSTGRES 只能是 true 或 false。" >&2
  restore_failed
fi

require_maintenance_lock || restore_failed

if ! MSYS_NO_PATHCONV=1 docker compose run --rm --no-deps -T \
  --name icloud-api-keys-maintenance \
  --entrypoint /usr/local/bin/icloud-api-keys-maintenance \
  icloud-api validate - < "$BACKUP_SET/icloud-api-keys.tar"; then
  echo "应用密钥归档预检失败；PostgreSQL 尚未恢复。" >&2
  restore_failed
fi
require_maintenance_lock || restore_failed

if [ "$RESET_EMPTY_POSTGRES" = true ]; then
  if ! docker compose run --rm --no-deps -T postgres prepare-restore; then
    echo "重置空 PostgreSQL 恢复状态失败。" >&2
    restore_failed
  fi
  require_maintenance_lock || restore_failed
fi

if ! docker compose up -d --wait postgres; then
  echo "PostgreSQL 未就绪。" >&2
  restore_failed
fi
require_maintenance_lock || restore_failed
if ! MSYS_NO_PATHCONV=1 docker compose exec -T postgres \
  /usr/local/bin/icloud-api-postgres-entrypoint restore \
  < "$BACKUP_SET/icloud-api.dump"; then
  echo "PostgreSQL 恢复失败。" >&2
  restore_failed
fi
require_maintenance_lock || restore_failed
if ! MSYS_NO_PATHCONV=1 docker compose run --rm --no-deps -T \
  --name icloud-api-keys-maintenance \
  --entrypoint /usr/local/bin/icloud-api-keys-maintenance \
  icloud-api restore - < "$BACKUP_SET/icloud-api-keys.tar"; then
  echo "应用密钥恢复失败。" >&2
  restore_failed
fi
require_maintenance_lock || restore_failed
if ! docker compose up -d --wait icloud-api; then
  echo "数据已恢复，但应用启动或健康检查失败。" >&2
  restore_failed
fi
require_maintenance_lock || restore_failed
if ! release_maintenance_lock; then
  echo "释放维护锁失败。" >&2
  restore_failed
fi
trap - EXIT HUP INT TERM
```

PostgreSQL `restore` 会动态读取目标安装新生成或已有的随机数据库名、用户和密码，并接受 stdin 或一个归档路径。它先把输入保存到权限受限的临时文件，检查 `PGDMP` 标识和非空归档目录，并要求当前结构的 11 张项目表、每张表的 `TABLE DATA` 段、四个身份序列及其 `SEQUENCE SET` 状态，以及全部项目主键、唯一约束、外键和八个关键查询索引都存在，再让 `pg_restore --clean --if-exists --no-owner --no-acl --exit-on-error` 把整份 custom archive 成功展开为 SQL；这些步骤都不会连接或修改目标数据库。内建 `backup` 不接受改变 dump 范围或格式的附加参数，因此不会生成可误恢复的 `schema-only` 或缺少 post-data 的归档。验证完成后，同一个 `psql --single-transaction` 会依次重建 `public` schema、执行恢复 SQL、通过 `pg_catalog` 再确认约束类型、外键有效状态及索引可用状态，并再次收紧 schema 权限；`ON_ERROR_STOP` 会让任意 SQL 或结构断言错误回滚整个事务。因此空归档、损坏归档、项目对象、数据段或 post-data 缺失和展开失败不会改变现有数据，有效归档在执行阶段失败也会保留恢复前的完整 schema。数据库备份与恢复共用独立的互斥锁，宿主维护窗口另由 `icloud-api-maintenance-lock` 覆盖全生命周期；归档由本项目的 `backup` 生成且不包含 `CREATE DATABASE`，恢复始终写入目标安装当前的随机数据库，不使用 `pg_restore --create`。

`prepare-restore` 通常只重置已丢失或为空的 `postgres_data`。如果首次 `initdb` 被中断，目录可能已经出现 `PG_VERSION` 或其他残留、但还没有持久化数据库凭据状态；此时正常启动会明确提示执行 `prepare-restore`。该命令持有与 PostgreSQL 主进程相同的非阻塞生命周期锁。清理非空 PGDATA 前，它要求数据库凭据、`postgres_config`、`installation_state` 和应用主密钥状态中的全部首次初始化标记都是无链接普通文件、安装 ID 完全一致、不存在任何完成标记，并要求配置卷与安装状态卷保存的“安装 ID + 当前 PGDATA 根目录设备号/inode”绑定同时匹配。旧配置卷或旧安装状态卷不能单独授权清空另一个数据卷。首次初始化状态在执行 `initdb` 前统一同步到持久卷；初始化脚本则先发布并同步 PGDATA 完成凭据，再发布并同步两个外部完成标记，最后移除旧 bootstrap/绑定标记并再次同步。健康数据库、来源不明的非空目录、自定义 `PGDATA` 路径或证明链不完整的旧版中断现场都会停止清理。`RESET_EMPTY_POSTGRES=true` 只用于目标卷仍为空，或入口明确判定首次 `initdb` 尚未完成且尚未成功启动 PostgreSQL 健康实例的场景；本轮只要 `prepare-restore` 已成功且随后 `docker compose up -d --wait postgres` 已建立健康集群，后续任何步骤（包括数据库 `restore`，即使该事务随后回滚、密钥恢复或应用健康检查）失败，保持应用停止并从第一行重跑整段脚本时，必须把该值设为 `false`，避免再次进入空卷准备流程。

密钥维护脚本接受归档路径，也接受 `-` 从 stdin 读取；两种输入都会先复制到权限受限的私有临时文件，再解到 `icloud_api_keys` 卷内的临时目录。它只接受 `master.key`、管理员正式/待提交密码及来源标记、OAuth Token 及来源标记，并拒绝未知路径、重复归档项、软链接、硬链接、互斥凭据冲突和格式错误。`validate` 与 `restore` 复用同一套归档复制、解包及环境凭据校验；`validate` 成功后只清理临时项，不创建恢复哨兵或回滚副本，不发布、删除正式密钥，也不调用 `admin reset`。恢复流程在任何 `prepare-restore` 或数据库 `restore` 前通过 stdin 执行该预检，正式密钥恢复时仍会从 stdin 重新复制并独立重验归档。完整校验通过后，`restore` 先发布 `admin-password.pending` 哨兵，再以同卷文件操作更新密钥；可捕获的失败会从同卷完整副本回滚，强制终止留下的哨兵会阻止普通应用启动。如果 keys 卷中存在 `.restore-old.*`，脚本会保留它并停止下一次恢复；先核对该回滚副本并将其移出 keys 卷，再重新执行同一条 `restore`。归档本身带有 `admin-password.pending` 时，维护脚本会在发布前确认宿主维护锁已由外部窗口持有，再忽略当前管理员密码环境变量并严格使用归档候选值调用应用入口完成 `admin reset`。这个内部调用只有在继承的文件描述符确实指向当前 keys 卷、keys 独占锁仍有效且宿主维护锁仍被占用时才能绕过重复加锁；普通外部环境变量不会跳过锁。只有数据库事务提交、待提交文件及旧来源标记均清除后，密钥恢复才返回成功。

恢复后登录后台并验证管理员凭据、OAuth 调用、主号凭据解密、IMAP 同步、邮件 API 和审计记录。原生部署同样应使用 `pg_dump`/`pg_restore` 备份 PostgreSQL，并同时保存 `ICLOUD_API_MASTER_KEY_FILE` 或密钥管理系统中的 `ICLOUD_API_MASTER_KEY`。

## 安全事项

- 生产环境应放在 HTTPS 反向代理之后，并设置 `ICLOUD_API_COOKIE_SECURE=true`。限制后台来源 IP，不要直接把无 TLS 的管理端口暴露到公网。
- `.env`、`postgres_config`、`icloud_api_keys`、OAuth Token、App 专用密码、受信任 Apple Web 会话、完整 API Key、PostgreSQL 数据/备份、旧 SQLite 数据和主密钥都属于敏感信息。限制文件/卷权限，不要写入代码、工单或普通访问日志。Apple 账户密码和双重认证验证码仅在连接流程中使用，不落库；会话过期后重新登录，不要尝试保存或复用验证码。外部登记接口必须通过 HTTPS 调用，怀疑 OAuth Token 泄露后应立即轮换并重启服务。`/api/v1/mail/recent` 为支持直接访问而有意把 Key 放入 URL；应优先使用不在 URL 暴露 Key 的 `/api/v1/mail/latest`，使用紧凑链接时必须把整个 URL 视为密钥并对代理日志中的查询字符串做删除或脱敏。
- 完整接口返回的 `html` 是未经清理的外部邮件内容，前端展示时必须做 HTML 清理或放入严格隔离的沙箱，禁止直接插入管理页面 DOM。紧凑接口回退到 HTML 正文时，`snippet` 只保留提取出的文本，不返回 HTML 的结构标签、样式或脚本。
- 默认优先使用 `X-ICLOUD-HME` 以及 `Delivered-To`、`X-Original-To`、`Envelope-To` 等投递头判断邮件归属。这些都是普通邮件头，本身没有密码学可信性；HME 路由头也必须经过上面的主号、可见收件人和物理投递收件人交叉校验。归属隔离仍依赖 iCloud 在实际 Hide My Email 转发链路中注入可识别的隐私邮箱投递标记，并清洗或隔离发件人预置的同名头。
- 上线前必须用真实 Hide My Email 原始邮件完成验收：既测试正常投递，也测试发件人预置同名 `Delivered-To`、`X-Original-To`、`Envelope-To` 等头的投递，确认 iCloud 最终保存的原始邮件仍能让服务唯一识别真实隐私邮箱。不同候选投递头指向不同地址时，该封邮件会作为不可安全归属的邮件跳过，不会写入任何隐私邮箱快照，也不会阻塞同批其他邮件；如果 Apple 没有注入可识别标记且保留了单个伪造同名头，仍存在邮件误归属的残余风险。
- `ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS` 默认为 `false`。有效的 `X-ICLOUD-HME` 路由不受这个开关影响；开关只会在缺少已配置投递标记时使用可由发件人影响的 `To`/`Cc`，会进一步降低隐私邮箱隔离强度。对候选 UID 窗口中发现、但 HME 头缺失、重复或校验失败的相关邮件，同步会报错且不会推进主号游标，也不会回退到 `To` 误归属。只有在真实样本验收并明确接受该风险后才应启用。
- Key 泄露后立即在后台轮换；App 专用密码泄露后应在 Apple 账户页面撤销，再在后台更新主号凭据。
- 应同时在反向代理层设置请求大小、速率和连接数限制。应用内 Key 限流保存在单个进程内，不替代边缘限流。
- 生产环境维持单个 `icloud-api` 副本运行。多个实例会重复轮询 IMAP，并拆分进程内同步互斥与限流状态。
- 定期做 PostgreSQL 逻辑备份并执行恢复演练。确认同一恢复集包含数据库备份、完整 `icloud_api_keys` 归档，以及所有由 `.env` 或 Secret 覆盖的应用凭据。
