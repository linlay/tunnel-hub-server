# Tunnel Hub Server

## 1. 项目简介

`tunnel-hub-server` 是 Tunnel Hub 的 Go 后端，负责公网 Relay、Agent/Desktop 出站隧道、管理 API、Desktop 注册 API、公开组件列表，以及基于 Host 的 HTTP/WebSocket 转发。

当前生产形态是拆分部署：

- `tunnel-hub-server`: 后端 Relay 和 API。
- `tunnel-hub-website`: React/Vite 管理前端，作为独立静态站点容器部署。
- `tunnel-hub-public`: Desktop public Host 的轻量浏览器客户端，作为独立静态站点容器部署。
- `tunnel-hub-tester`: 本地 Desktop WebSocket 调试台，不参与生产流量。

典型域名规划：

- `hub.example.test`: 管理前端、`/api/admin`、`/api/desktop`、`/api/components` 和 `/tunnel`；不提供附件业务 API。
- `*.m.example.test`: 普通设备 Host 打开 Desktop public mini site；`<device>-<frontendPort>.m.example.test` 的全部请求，以及普通设备 Host 的 WebSocket upgrade、`POST /api/upload` 和 `GET /api/resource` 请求进入 Relay。
- `*-wa.example.test`: Desktop WebApp 反向代理入口，支持 HTTP 和 WebSocket，并由 `*.example.test` 单层泛域名覆盖。
- `share.example.test`: 分享的公开只读 origin；边缘网关将 `/share/*` 和 `/assets/conversation-export/*` 转发到 Relay。前者按冻结描述返回内置对话、隔离业务预览或已支持动作，后者返回对话渲染资源。

WebApp 有两条独立链路：

- 手机配对访问使用 `https://<device>-<frontendPort>.m.example.test/`，不创建数据库 WebApp route。首次导航携带配对 `app` token，Relay 换成 HttpOnly Cookie 并重定向到无 token 地址；每个请求仍由 Desktop 校验 token scope、device id 和运行中的 WebApp 端口。
- 用户主动公开分享使用 `*-wa.example.test`。该路由仅由 Desktop 的“一键发布”注册和启停，保持匿名 URL 分享语义。

## 2. 快速开始

### 前置要求

- Go 1.25
- MySQL 8.0.16+（InnoDB，`max_allowed_packet >= 67108864`）或本地 SQLite 文件
- Docker / Docker Compose
- OpenSSL，可选，用于从官网 SSO 私钥导出 JWT 公钥
- 一个可用的官网 SSO JWT 公钥，生产和 Desktop 注册 API 必需

### 本地启动 Relay

```bash
cd tunnel-hub-server
cp .env.example .env
make test
make run-relay
```

复制 `.env.example` 后，在已忽略的 `.env` 中填写 `GO_MODULE_PATH`、运行时身份、域名、Relay 地址、数据库类型及 SSO 校验参数。MySQL 是默认存储；单实例部署也可以显式选择 SQLite。仓库不提交任何环境的真实身份值。

Relay 启动要求同时提供 SSO issuer、audience、用户 ID claim，以及文件或 PEM 形式的有效 JWT 公钥。缺少任何一项都会在监听端口前失败并指出字段。

如果需要启用本地管理账号，在 `.env` 中设置：

```bash
ADMIN_USERNAME=admin
ADMIN_PASSWORD=<local-password>
```

`ADMIN_PASSWORD` 为空时不会自动创建本地管理员。

### 启动 Agent

普通 Agent 使用已创建的 tunnel token。Desktop 先通过统一认证注册设备，连接 `/tunnel` 时在首个 `tunnel.open` 中发送 `identityToken + deviceId`，不再创建或保存第二份 Relay token。

```bash
cd tunnel-hub-server
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:11961/tunnel make run-agent
```

生产环境使用：

```bash
AGENT_RELAY_URL=wss://hub.example.test/tunnel
```

### 本地容器运行

```bash
cd tunnel-hub-server
cp .env.example .env
docker compose up --build
```

`docker-compose.yml` 会把 `.env` 中的 `GO_MODULE_PATH` 作为构建参数传给 server，并把运行时配置注入 server/public；默认 Relay 连接外部 MySQL，JWT 公钥以只读文件挂载；Public 容器只接收 `PUBLIC_SITE_TITLE`。启动前请确认 `.env` 和 `configs/jwt-public.pem` 均已准备好。

SQLite 单实例部署使用同一镜像和可选覆盖文件：

```bash
docker compose -f docker-compose.yml -f docker-compose.sqlite.yml up -d --build
```

该覆盖文件把 `./data` 挂载到容器并使用 `/data/relay.sqlite`。不要让多个 Relay 实例或主机同时使用同一个 SQLite 文件。

## 3. 配置说明

本项目会自动加载当前工作目录下的 `.env`。真实 shell 环境变量或容器环境变量优先级高于 `.env`。不要提交真实密钥、token、密码或生产 JWT key material。

### 运行时身份配置

Relay 和 `tunnel-hub-public` 都在进程或容器启动时读取环境变量，不在源码或镜像构建层中注入真实值：

| 名称 | 必填 | 说明 |
| --- | --- | --- |
| `BRAND_ID` | 是 | 稳定部署身份，只允许小写字母开头以及小写字母、数字、连字符。该值派生 mobile session Cookie，投产后不要随意修改。 |
| `PRODUCT_NAME` | 是 | 产品显示名。 |
| `PUBLIC_SITE_TITLE` | 是 | Public 页面运行时标题；Nginx 容器缺少该值时拒绝启动。 |
| `PUBLIC_BASE_DOMAIN` | 是 | Tunnel API/管理入口 hostname。 |
| `DESKTOP_PUBLIC_BASE_DOMAIN` | 是 | Desktop public wildcard 根 hostname。 |
| `WEBAPP_PUBLIC_BASE_DOMAIN` | 是 | WebApp 单层 wildcard 根 hostname；随机 Host 生成格式为 `<id>-wa.<该值>`。可与 `PUBLIC_BASE_DOMAIN` 相同，但必须与 `DESKTOP_PUBLIC_BASE_DOMAIN` 不同。 |
| `RELAY_PUBLIC_URL` | 是 | Relay WebSocket URL；非 Loopback 地址必须使用 `wss` 和 `/tunnel`。 |
| `SHARE_PUBLIC_BASE_URL` | 是 | 公开分享 origin；非 Loopback 地址必须使用 HTTPS，本地 Loopback 可使用 HTTP。 |
| `PRODUCT_DOWNLOAD_PAGE_URL` | 否 | 公开分享页的产品介绍及下载地址。非 Loopback 地址必须使用 HTTPS；留空时分享页仅尝试唤起桌面应用。 |
| `CONVERSATION_SHARE_RESOURCE_DIR` | 是 | 对话分享资源的绝对路径；必须可写并位于持久化卷，文件正文保存在此目录，数据库仅保存元数据。 |

三个 domain 都不接受 scheme、端口、路径、通配符或 IP。`PUBLIC_BASE_DOMAIN` 可与 `WEBAPP_PUBLIC_BASE_DOMAIN` 相同；Desktop public 根域必须与另外两个不同。Relay 会严格校验全部值，缺失或非法时在监听端口前失败。

HTTPS 下 mobile session Cookie 为 `__Host-<BRAND_ID>_mobile_session`，本地 HTTP 下为 `<BRAND_ID>_mobile_session`。

### Go module 构建身份

提交态 `go.mod` 和内部 import 固定使用中性路径 `example.invalid/tunnel-hub-server`。Go import 不能读取环境变量，因此 `make` 和 Docker 构建入口会读取 `.env`/CI 中的 `GO_MODULE_PATH`，复制未忽略的工作树到临时目录，仅在临时树中替换 module path 后执行 build/test/run；源码工作区不会被改写。

GitHub 和 GitLab CI 应分别设置各自仓库对应的 `GO_MODULE_PATH`。同时从 CI 外部提供非空 `FORBIDDEN_BRAND_TERMS`，通过 `make verify-neutral` 对文件路径和内容做大小写不敏感扫描；缺少禁用词配置时检查会失败。

### Relay 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `RELAY_ADDR` | 必填 | Relay 本地监听地址；示例为 `:11961`，容器内由 Compose 覆盖为 `:8080`。 |
| `ADMIN_HOST` | 空 | 旧版 Relay 静态管理站点 Host；拆分部署时保持为空。 |
| `WEBSITE_DIST` | 空 | 旧版 Relay 静态站点目录；拆分部署时保持为空。 |
| `ADMIN_USERNAME` | `admin` | 本地管理账号 bootstrap 用户名。 |
| `ADMIN_PASSWORD` | 空 | 本地管理账号 bootstrap 密码；为空时跳过创建。 |
| `ADMIN_SESSION_TTL` | `24h` | 本地管理登录 cookie 有效期。 |
| `COOKIE_SECURE` | `false` | 管理 cookie 是否只允许 HTTPS。生产 HTTPS 下建议设为 `true`。 |
| `MOBILE_WEBAPP_COOKIE_SECURE` | `true` | `.m` WebApp 会话 cookie 是否只允许 HTTPS；仅本地 HTTP 联调时设为 `false`。 |
| `SSO_JWT_ISSUER` | 必填 | 官网 SSO JWT issuer。 |
| `SSO_JWT_PUBLIC_KEY_FILE` | 二选一 | 官网 SSO JWT PEM 公钥文件路径。 |
| `SSO_JWT_PUBLIC_KEY_PEM` | 二选一 | 官网 SSO JWT PEM 公钥内容，支持转义 `\n`。 |
| `SSO_JWT_AUDIENCE` | 必填 | JWT audience。 |
| `SSO_JWT_USER_ID_CLAIM` | 必填 | 用作稳定用户标识的 JWT claim 名。 |
| `SSO_JWT_ALLOW_ANY_AUDIENCE` | `false` | 兼容开关；为 `true` 时跳过 audience 校验，但仍校验签名、issuer 和有效期。 |
| `SSO_JWT_ALLOW_ANY_ADMIN_ROLE` | `false` | 高风险兼容开关；为 `true` 时任意有效 SSO 用户都获得 Tunnel Hub 管理权限。 |
| `SSO_JWT_ALLOW_MISSING_TUNNEL_SCOPE` | `false` | 兼容开关；为 `true` 时管理、Desktop 注册和 Desktop Tunnel 握手不再要求 `scope=tunnel`。 |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | Relay 缓冲 HTTP 请求体的最大字节数。 |
| `TRUSTED_PROXY_CIDRS` | 空 | 可信反向代理 CIDR，命中后才读取 `X-Real-IP` / `X-Forwarded-For`；生产 Docker + nginx 建议 `172.23.0.1/32,127.0.0.1/32,::1/128`。 |

### 数据库环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `DATABASE_TYPE` | `mysql` | 可选 `mysql` 或 `sqlite`；缺省仍使用 MySQL。 |
| `RELAY_DB_PATH` | 无 | SQLite 模式必填；父目录必须已存在且可写。MySQL 模式忽略。 |

数据库类型只在启动时选择。连接失败不会自动回退到另一种数据库，两种数据库之间也不会自动同步或迁移数据。

### MySQL 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `MYSQL_HOST` | 必填 | 数据库主机；容器内必须使用容器可访问的地址，`127.0.0.1` 指向容器自身。 |
| `MYSQL_PORT` | `3306` | 1–65535。 |
| `MYSQL_DATABASE` | 必填 | 预先创建的专用数据库。 |
| `MYSQL_USER` | 必填 | 专用数据库账号。 |
| `MYSQL_PASSWORD` | 必填 | 原样读取；`.env` 中带首尾空格或特殊字符时使用单引号。 |
| `MYSQL_TLS` | `true` | 校验证书和主机名；可信本机/内网连接可显式设为 `false`。 |
| `MYSQL_TLS_CA_FILE` | 空 | 可选私有 CA PEM 文件，仅 TLS 开启时可用。容器使用时需只读挂载并填写容器内路径。 |
| `MYSQL_MAX_OPEN_CONNS` | `10` | 正整数。 |
| `MYSQL_MAX_IDLE_CONNS` | `5` | 0 到最大连接数之间。 |

应用账号仅需专用库内 `CREATE`、`REFERENCES`、`SELECT`、`INSERT`、`UPDATE`、`DELETE` 权限。由数据库管理员按部署地址创建库和用户并授权，例如：

```sql
CREATE DATABASE tunnel_hub CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
CREATE USER 'tunnel_hub'@'<relay-host>' IDENTIFIED BY '<replace-password>';
GRANT CREATE, REFERENCES, SELECT, INSERT, UPDATE, DELETE
  ON tunnel_hub.* TO 'tunnel_hub'@'<relay-host>';
```

服务端须配置 `max_allowed_packet=64M` 或更高。应用启动校验版本、连接与传输上限，不满足时在监听前失败。连接超时 5 秒、读写超时各 30 秒，连接最大生命周期 3 分钟。时间统一使用 UTC `DATETIME(6)`，精度为微秒；标识符按大小写区分，文本使用 `utf8mb4`。Snapshot JSON 保存在数据库中，新建额度由 `CONVERSATION_SHARE_MAX_SNAPSHOT_MIB` 配置（默认 50 MiB）；资源正文仅保存在 `CONVERSATION_SHARE_RESOURCE_DIR`，每个新分享的资源累计额度由 `CONVERSATION_SHARE_MAX_RESOURCES_MIB` 配置（默认 50 MiB），数据库只保存资源元数据。旧版 `conversation_share_attachments` BLOB 表由可重跑的 schema 迁移写入文件目录并删除；升级期间数据库账号还需临时具备 `DROP` 权限。

MySQL 启动逐表执行幂等建表；中途失败可以修复原因后重启，已有成功创建的表保留。MySQL DDL 不保证整套 schema 事务回滚，现有连接池、TLS、字符集和并发事务语义保持不变。

SQLite 使用 WAL、外键约束、5 秒 busy timeout 和单连接池。新库在一个事务中建立当前 schema 并写入版本；带有历史表但没有当前版本标记的旧 SQLite 文件会被拒绝，不执行旧字段修补或历史数据迁移。

### Agent 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_RELAY_URL` | `ws://127.0.0.1:11961/tunnel` | Agent 默认 Relay tunnel WebSocket 地址。 |
| `AGENT_TOKEN` | 必填 | 普通 Agent tunnel token。 |
| `AGENT_TLS_INSECURE_SKIP_VERIFY` | `false` | 开发调试 TLS 跳过校验开关，生产不要开启。 |
| `AGENT_RECONNECT_SECONDS` | `3` | 断线重连间隔。 |

### SSO JWT 公钥

建议把官网 SSO 公钥放在 `configs/jwt-public.pem`，并通过 `SSO_JWT_PUBLIC_KEY_FILE=configs/jwt-public.pem` 或容器内 `/configs/jwt-public.pem` 使用。

从官网 SSO 私钥导出公钥：

```bash
mkdir -p configs
openssl pkey -in /path/to/official-sso-private.pem -pubout -out configs/jwt-public.pem
```

## 4. 部署与打包

### 构建二进制

```bash
cd tunnel-hub-server
make build
```

### 构建镜像

```bash
cd tunnel-hub-server
make docker-build
docker build -f tunnel-hub-public/Dockerfile -t tunnel-hub-public:example .
```

Server 镜像会同时构建 `/app/relay` 和 `/app/agent`，默认入口是 `/app/relay`。直接执行 `docker build` 时必须显式传入 `--build-arg GO_MODULE_PATH=...`；可用 `GO_IMAGE` 和 `RUNTIME_IMAGE` build args 覆盖基础镜像。构建阶段固定 `GOTOOLCHAIN=local`，不会自动下载其他 Go 工具链。

### Docker Compose

```bash
cd tunnel-hub-server
docker compose up -d --build
```

默认映射 `127.0.0.1:11961:8080`，避开 Desktop OIDC 的本地 8080 端口。生产宿主机可以改为只监听内网端口，再由 Nginx/Caddy 终止 TLS 并转发。

### 拆分生产部署

推荐部署拓扑：

- Relay: `127.0.0.1:11961 -> 8080`
- Website: `127.0.0.1:11963 -> 80`
- Public Desktop site: `127.0.0.1:11965 -> 80`
- `hub.example.test/`: 转发到 website 容器。
- `hub.example.test/api/admin`, `/api/desktop`, `/api/components`, `/tunnel`: 转发到 Relay；`/api/upload`、`/api/resource` 和旧 `/api/download` 明确返回 404。
- `*.m.example.test`: `<device>-<frontendPort>.m.example.test` 的全部路径，以及普通设备 Host 的 WebSocket upgrade、`POST /api/upload` 和 `GET /api/resource` 转发到 Relay；普通 `<device>.m.example.test` HTTP 转发到 public Desktop site。
- `*-wa.example.test`: 由 `*.example.test` 通配符入口直接转发到 Relay。
- `share.example.test/share/*` 由公开边缘网关直接转发到 Relay。Relay 查询当前配置的数据库后返回 HTML；`/assets/conversation-export/*` 在分享 origin 和 Tunnel API origin 都返回编入 Relay 的不可变 JS/CSS/font 资产，供线上分享、落盘导出和本地 loopback 环境复用。HTML 资源 origin 由 Desktop Worker 按当前 Tunnel 配置注入，不绑定固定域名。

Tunnel 端模板在 `deploy/nginx/tunnel-hub.conf.template` 和 `deploy/caddy/Caddyfile.template`，其中包含分享 origin 的 `/share/*`，以及分享/Tunnel API 两个 origin 的 `/assets/conversation-export/*` 直连 Relay 规则。上线前必须替换全部 `{{...}}` 占位符；分享关闭时删除分享 Host block。

Relay 在 `/tunnel` 完成握手后每 20 秒发送一次空载荷的标准 WebSocket Ping，Agent 和 Desktop 必须按 WebSocket 协议返回 Pong。反向代理、负载均衡器、NAT 和防火墙的 WebSocket 空闲超时都必须大于 20 秒；生产模板使用更长的超时，避免空闲隧道被中间链路误判为失活。

| 模板占位符 | 环境变量来源 |
| --- | --- |
| `{{PUBLIC_BASE}}` | `PUBLIC_BASE_DOMAIN` |
| `{{DESKTOP_PUBLIC_BASE}}` | `DESKTOP_PUBLIC_BASE_DOMAIN` |
| `{{WEBAPP_PUBLIC_BASE}}` | `WEBAPP_PUBLIC_BASE_DOMAIN` |
| `{{SHARE_HOST}}` | `SHARE_PUBLIC_BASE_URL` 的 hostname |
| `{{DESKTOP_PUBLIC_BASE_REGEX}}` | 转义正则元字符后的 `DESKTOP_PUBLIC_BASE_DOMAIN` |
| `{{ACME_EMAIL}}`、`{{*_CERTIFICATE}}`、`{{*_CERTIFICATE_KEY}}` | 部署环境的 ACME 联系邮箱和证书绝对路径 |

生产 WebApp 入口使用同时覆盖 `WEBAPP_PUBLIC_BASE_DOMAIN` 根域及其单层 wildcard 的证书，不需要二级 wildcard 证书。

## 5. 运维

### 常用检查

```bash
make test
make verify-neutral
docker compose ps
docker logs tunnel-hub-server
```

### 数据与备份

- MySQL 数据的备份与恢复使用部署环境的 MySQL 运维流程。SQLite 部署应在停止 Relay 或完成一致性快照后备份数据库文件及 WAL 状态；数据库与 `CONVERSATION_SHARE_RESOURCE_DIR` 必须属于同一备份时点，保留历史对话渲染资产。
- MySQL 模式只在已存在的专用库中创建当前表结构，不创建数据库或账号；SQLite 模式创建配置路径对应的新文件。已有库只执行源码列出的显式迁移，包括旧非永久分享先撤销再删退役字段；无法识别的旧结构应停止升级并保留原数据。
- 首次上线顺序：准备 MySQL 空库和账号或 SQLite 可写父目录 → 配置环境变量 → 停止旧 Relay → 启动新 Relay 建表 → 初始化管理员 → 验证注册、路由、分享。
- 历史数据库文件和 Docker 卷不自动删除。数据库类型切换会看到另一套独立数据，不会同步账号、Token、设备映射或分享链接。

### 常用 API

本地管理账号登录：

```bash
curl -i http://127.0.0.1:11961/api/admin/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<local-password>"}'
```

使用官网 SSO JWT 发布普通服务：

```bash
curl -X PUT https://hub.example.test/api/admin/services/auditor \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:3000","tokenId":"token_...","active":true}'
```

注册 Desktop：

```bash
curl -X POST https://hub.example.test/api/desktop/devices/register \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT" \
  -H "Content-Type: application/json" \
  -d '{"deviceId":"mac-mini","deviceName":"Frank MacBook Pro"}'
```

通用分享 v2 使用 `POST/GET /api/desktop/v2/shares`、`GET /api/desktop/v2/shares/limits` 和 `DELETE /api/desktop/v2/shares/{id}`，继续由管理 SSO 取得所有者，不接受 userId 或旧对话请求头。创建 multipart 第一部分为 application/json 的 `manifest`，资源部分为 `resource:<id>`；字段以 `internal/sharecontract` 为准。v2 限额中的 maxManifestBytes 复用原正文额度，资源预算、staging、提交及失败补偿与旧协议共用；每次 POST 生成独立 ID。列表只查标题、类型、创建/访问时间等元数据，不加载内容或描述 BLOB。

跨端用户操作与来源业务、插件/WebApp、在线服务接入见 sibling Desktop 的 [分享短链使用与对接](../zenmind-desktop/contracts/share-shortlink.md)。当前 Nginx 模板的 v2 创建路径虽然由 `/api/desktop/` 转发，却缺少旧分享上传入口的大小/请求缓冲覆盖；默认 100m 网关上限与 Hub 默认 101 MiB 总请求额度不一致，部署前需补齐并验证，不能把路由可达当作上传策略已经一致。此问题已核实但本轮未修改模板，不代表线上生效配置已复现。

公开 `/share/{id}` 默认 preview，`?action=resolve` 返回最小 JSON，`?action=download&resourceId={id}` 读取本分享资源，`?action=desktop` 提供品牌分享接收链接。选择参数重复、未知动作、资源越界或 receiver/context/version、设备和在线绑定等覆盖请求均失败；同一 ID 所有行为共用撤销状态。资源公开读取复验长度和 SHA-256，禁止重定向和任意服务器路径。当前支持内置对话、安全元数据页、显式自包含预览包及已有 WebApp 发布绑定。

`service:{type:"webapp",webAppId}` 只引用现有注册 ID；创建时核实发布仍 active、设备未撤销且属于分享所有者。公开 `?action=service` 再检查同一分享撤销状态与当前绑定，303 跳转至原注册公网 Host；不把分享站当代理、不接收 upstream、不转发所有者 SSO。当前服务、离线 502、HTTP/WebSocket 与访客权限继续由原 WebApp 发布链路处理。停止发布或移除绑定返回明确不可用页并保留冻结预览。撤销分享只关闭该短链及其私有资源，原公开地址和其他分享不受影响；停止公开服务仍使用原发布操作。

自包含包的 `preview.entrypoint` 必须命中本分享资源的逻辑路径且为 text/html。可信外层使用 `sandbox="allow-scripts"` 的 opaque iframe，配合 credentialless、no-referrer 和仅限本分享 preview 路径的 frame-src；外层不运行包脚本或处理包消息。`/share/{id}/preview/{path}` 每次检查描述、清单归属、撤销及文件 Hash，返回严格 CSP 和无凭据 CORS，模块/JSON/字体/图片可加载。脚本、样式、图片、字体及连接仅允许本包资源路径；禁止任意网络、嵌套 frame、Worker、表单、弹窗、顶层导航与包内下载。包资源要求浏览器 Fetch Metadata，拒绝 document/object/worker 或缺失目的地的读取，保证 SVG/XHTML 等可执行文档也不能作为顶层页面绕开外层导航限制。普通资源下载仍走 action=download；普通 HTML 附件仍清洗并禁用脚本。

预览 CSP 使用配置的 `SHARE_PUBLIC_BASE_URL`，应通过记录返回的 canonical URL 浏览包。此方案已在 macOS Chrome 156 实测；其他平台/浏览器需按 QA 验证。手工浏览器回归夹具为 `internal/desktop/share_preview_browser_test.go`：显式设置独立临时 QA 目录、临时 TMPDIR 和 SQLite 后运行 TestSharePreviewBrowserHarness；常规测试不启动该服务。包样例只用于验证，不是新业务应用。未修改 WebClient 导出源码或已编译资源集。

创建、读取和撤销对话分享：

Relay 保存 Desktop 上传的 `ConversationSnapshotV1` JSON 和冻结的 HTML 附件，匿名访问时安全注入当前唯一分享模板。Snapshot 与附件累计分别默认 50 MiB，环境配置只接受正整数 MiB，重启生效。创建前通过同一鉴权读取 `GET /api/desktop/shares/limits` 的 `maxSnapshotBytes`、`maxResourceBytes`（字节，`no-store`）。请求体额度由两者加 1 MiB 推导，Go 上传入口执行限制，独立于通用请求额度；MySQL packet 至少 64 MiB 且容纳快照加 1 MiB。store 和历史读取不重复执行新建额度。创建请求必须提供 `multipart/form-data`、`X-Conversation-Snapshot-Version: 1`、非空 `X-Conversation-ID` 和 `X-Conversation-Share-Expiration`；该兼容请求头只接受 `permanent`；`once`、`3h`、`1d`、`7d`、`30d` 明确拒绝，不转为永久。multipart 中 `snapshot` part 必填，每个附件使用 `attachment:<id>` part 并与 Snapshot 声明严格匹配。Desktop 不下载模板或生成分享 HTML；本地 HTML 导出仍是独立功能。下面命令只用于服务端联调。

```bash
curl -X POST https://hub.example.test/api/desktop/shares \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT" \
	-H "X-Conversation-Snapshot-Version: 1" \
	-H "X-Conversation-ID: chat_xxx" \
	-H "X-Conversation-Share-Expiration: permanent" \
	-F "snapshot=@conversation-snapshot.json;type=application/json" \
	-F "attachment:0123456789abcdef01234567=@report.html;type=text/html"

curl https://hub.example.test/api/desktop/shares \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT"

curl https://share.example.test/share/share_xxx

curl -I https://share.example.test/assets/conversation-export/<asset-set-hash>/runtime.js

curl -X DELETE https://hub.example.test/api/desktop/shares/share_xxx \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT"
```

创建和列表响应固定保留 id、conversationId、url、createdAt、expiresAt、lastAccessedAt、singleUse 七键，其中 expiresAt 固定为 null、singleUse 固定为 false。这两个兼容值只在 HTTP DTO 中存在。列表按创建时间倒序返回当前所有者仍有效的对话元数据，不读取内容 BLOB，也不接受查询参数。匿名 `GET /share/{id}` 使用当前模板渲染未撤销的 Snapshot；成功 GET best-effort 更新独立访问元数据，HEAD 不更新。公开读取不创建 Cookie 或领取会话。已撤销和未知 ID 统一返回最小 404 HTML。

分享持续有效直到所有者撤销。SQLite schema v5 与 MySQL 可重跑迁移先撤销所有旧 expires_at 非空或 single_use 为真的记录（保留已有 revoked_at），再移除退役列、CHECK 和 claims 表；旧永久内容字节、ID、创建/访问时间及资源目录不变。旧库备份必须连同资源目录保存，升级不能直接降级运行旧 Hub。新 kind/title/description_json 保存在原 conversation_shares 聚合内，快照仍只保存一份；conversation_id 可空，资源仍由 shareId 独占。管理列表不读 description_json 或 snapshot_json。撤销先落库，再删除文件，失败保留元数据；定期清理只回收已撤销资源及失败上传孤儿，不按时间使分享失效。

恢复旧备份时，先在隔离副本恢复匹配的数据库和资源，以当前迁移重新应用旧非永久记录的撤销，再验证永久 ID、正文和资源 Hash 后才可开放读取。恢复包含旧有效状态的备份并直接启动旧 Hub 会重新公开已经退役的链接，不能作为回退方式。`TestSQLiteShareBackupRestorePreservesPermanentAndReappliesRevocation` 用离线合成库验证上述恢复顺序及备份未被修改；它不代表生产备份或 MySQL 恢复已验证。

`GET/HEAD /assets/conversation-export/{sha256}/{file}` 提供随 Relay 编译且通过内容 Hash 校验的资源集；当前 manifest 决定新分享页使用的资源集，历史资源集继续服务已有 HTML。分享渲染包由 WebClient 显式同步后随 Relay 原子发布，普通 WebClient 发布不修改它。

公开分享页和可下载模板均在每次响应时生成独立的 32 字节随机样式 nonce，并同时填入 CSP 与 `conversation-export-style-nonce` 元数据。分享运行时将 nonce 传给组件动态样式，`style-src-elem` 不放开任意内联样式；可下载模板保留原有 Snapshot、资产 origin 和本地品牌占位符，旧版 Desktop 组装流程无需更改。

数据库升级采用明确的增量迁移：保留既有账号、设备映射、公网地址和分享；数据库类型切换不会自动复制数据，不提供双库回退逻辑。升级前先备份并在副本验证。

注册 Desktop WebApp：

```bash
curl -X PUT https://hub.example.test/api/desktop/devices/mac-mini/webapps/notes \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:5173","active":true}'
```

同一路径的已鉴权 `GET` 只读查询当前所有者已有的 active 发布，不注册或更新路由；响应新增 `webAppId`，供分享绑定使用。未知、其他用户、已撤销设备或停止发布的绑定统一不可取得，且不返回分享公开响应中的 owner 或 upstream。

上传附件到 Desktop chat：

```bash
curl -X POST https://device.m.example.test/api/upload \
  -H "Authorization: Bearer $DESKTOP_APP_TOKEN" \
  -F chatId=chat_xxx \
  -F file=@./note.txt
```

从 Desktop chat 下载附件：

```bash
curl -OJ 'https://device.m.example.test/api/resource?file=chat_xxx%2Fnote.txt' \
  -H "Authorization: Bearer $DESKTOP_APP_TOKEN"
```

公开组件列表：

```bash
curl https://hub.example.test/api/components
```

### 常见排查

- `official JWT verifier is not configured`: 检查 `SSO_JWT_ISSUER` 和 JWT 公钥配置。
- 启动时报 JWT 公钥文件不存在：准备有效公钥，并确认 `SSO_JWT_PUBLIC_KEY_FILE` 指向正确文件。
- 管理台无法登录：确认 `ADMIN_PASSWORD` 首次启动时已设置，或使用官网 SSO JWT 调用 API。
- `desktop is offline` / `assigned desktop is offline`: 确认 Desktop 已用有效统一认证 identity token 和已注册 deviceId 连接 `/tunnel`；普通 Agent 仍需 active tunnel token。
- WebSocket 无法升级：检查反向代理是否保留 `Upgrade` 和 `Connection` 头。
- Desktop public mini site 没有打开：确认 `*.m.example.test` 普通 HTTP 已转发到 `tunnel-hub-public`，不是 Relay。
- 附件上传返回 `desktop is offline`：确认请求 Host 对应的 Desktop 已连接 `/tunnel`。
- 附件资源返回 `desktop resource timed out`：确认 Desktop 已实现 `/api/resource` 业务帧，并能访问 Hub 提供的 ticket 保护回推 URL。
- 公网 Host 404：检查 DNS wildcard、Nginx/Caddy wildcard route，以及三个 domain 环境变量。

### 跨应用发布检查

- Desktop 注册响应字段 `relayUrl`、`publicHost`、`publicUrl`、`webSocketUrl` 保持不变；Desktop 与移动 WebApp 继续动态消费这些值。
- 发布前核对对应环境仓库的 `tunnelHub.relayUrl` 与 `RELAY_PUBLIC_URL` 完全一致，本仓库不修改 sibling 应用。
- `tunnel-hub-tester` 的远程附件 helper 仍限制在其既有域名，非当前品牌环境只保证可手工填写 URL 做普通 WebSocket 调试，附件适配另行处理。
- 顺序为：在部署环境准备完整运行时变量，渲染代理模板，最后同时发布 Relay、public 与代理配置。应用按所选数据库的当前 schema 初始化专用空库或新 SQLite 文件；不要指向已有其他业务数据的库。
- HTTP 上传失败：检查 `MAX_REQUEST_BODY_BYTES`，当前 Relay 会完整缓冲请求体。

## 6. 开发命令

```bash
make test
make test-sqlite
GO_MODULE_PATH=example.invalid/tunnel-hub-server GOTOOLCHAIN=local go run ./tools/moduleprep exec -- go test ./internal/proxy -run Test
GO_MODULE_PATH=example.invalid/tunnel-hub-server GOTOOLCHAIN=local go run ./tools/moduleprep exec -- go test ./internal/admin -run Test
make verify-neutral
gofmt -w ./cmd ./internal
```

`make test` 保持 MySQL 回归语义，必须连接独立 MySQL 8.0 测试服务；账号需能创建/删除 `tunnel_test_*` 临时库，并拥有库内 DDL/DML 权限。测试从不读取生产 `MYSQL_*`，不接受固定测试库名，每个测试使用随机命名的库并在结束时清理。缺少配置或清理失败均使测试失败，不会跳过。

```bash
export TEST_MYSQL_HOST=127.0.0.1
export TEST_MYSQL_PORT=3306
export TEST_MYSQL_USER=tunnel_test
export TEST_MYSQL_PASSWORD='<test-password>'
export TEST_MYSQL_TLS=false
# 若测试连接使用私有 CA：TEST_MYSQL_TLS=true，同时设置 TEST_MYSQL_TLS_CA_FILE。
make test
GO_MODULE_PATH=example.invalid/tunnel-hub-server GOTOOLCHAIN=local go run ./tools/moduleprep exec -- go test -race ./internal/store ./internal/admin ./internal/desktop ./internal/proxy
make build
make verify-neutral
```

SQLite 回归不依赖外部数据库，使用临时文件并在测试结束时清理：

```bash
make test-sqlite
# 配置 TEST_MYSQL_* 后同时运行两套：
make test-all
```

测试账号应仅在独立测试实例使用，不得授予应用生产账号全局建库/删库权限。测试异常中断后可检查并清理本次测试创建的 `tunnel_test_*` 库；归属不明的库不要删除。

提交前至少运行 `make test-all` 和 `make verify-neutral`。协议、转发、鉴权、配置、存储相关改动需要补充或更新对应 `*_test.go`。

### Account mobile routes

Hub owns Desktop membership, names, revocation and live Tunnel presence. GET `/api/desktop/account-devices` returns the current account's valid Desktops with online state; PATCH/DELETE `/api/desktop/account-devices/{deviceId}` rename/unbind them. Desktop registration reuses its persisted device ID and canonical OAuth SSO identity, without calling Identity for a second device registration.

POST `/api/desktop/route-credentials` accepts only `targetDesktopId` with the official SSO bearer proof. The configured `SSO_JWT_USER_ID_CLAIM` determines the owner; `account_id` never overrides it. The response returns that live Desktop's uploaded original Platform credential, local `platformDeviceId`, expiry and exact WebSocket endpoint with `subprotocol` transport. Offline returns 409; a missing/expired uploaded credential returns 503; other owners are denied. Responses use `Cache-Control: no-store`.

Desktop uploads optional `platformCredential: {token, platformDeviceId, expiresAt}` in `tunnel.open`. It refreshes the same live connection with POST `/api/desktop/devices/register` using `{deviceId, tunnelSessionId, platformCredential}` and its current official bearer proof. An update cannot register a computer, replace another session, change the local Identity device or overwrite a newer expiry. The existing Manager lock owns credentials; disconnect/replacement/revocation removes their accessibility. The Hub validates structural fields; Desktop/Platform validates the original Identity signature. No business credential is stored in the database or minted by the Hub.

Mobile Desktop WS, `/api/upload` and `/api/resource` require separate `X-Account-Authorization: Bearer <official-proof>` and the original business bearer token/subprotocol. Target ownership is checked before forwarding and before buffering uploads; the official proof is removed before downstream delivery. URL token authentication is rejected. Ordinary Agent and published WebApp routes retain their original contracts.

Controlled mobile WebApp hosts reserve POST `/api/mobile-session` and POST `/api/mobile-session/logout`. Bootstrap validates both headers and returns an opaque, host-only, Secure, HttpOnly, SameSite=Strict Cookie plus `{expiresAt}`. Its server-side memory entry is tied to the live Desktop and bounded by both proof and business token expiries. Page, subresource and WebSocket requests use that Cookie; the gateway removes it before forwarding the original business token internally. Logout expires it and removes the entry; a foreign Origin is refused. Raw business tokens never enter WebApp URLs or page headers. Native Cookie sharing must be accepted on real iOS/Android devices. The old `tr_` issuer, validation endpoint and Identity `/me` dependency have been removed.

SQLite v3 -> v4 and existing MySQL databases add only desktop_devices.revoked_at for this change. Revocation retains device/session/share history and public addresses, closes the Tunnel and discards in-memory credentials/Cookie sessions; registration using the old login session cannot restore the record; only a session created after revocation can restore the existing record. Missing official verifier/Manager state fails closed. Historical shares, snapshots and attachment files are unchanged.

### 账号设备生命周期

Desktop 的设备 ID 属于本地安装，普通退出和重连不撤销注册或改变 ID；同账号登录复用原设备、自定义名称和公网地址。手机主动移除使用设备 DELETE 接口，撤销并关闭在线连接。只有官网签名的 `session_started_at_ms` 严格晚于 `revoked_at`，注册事务才恢复原记录；缺少有效登录时间或仅刷新旧会话令牌均不能恢复。官网提供原始会话创建时间，Hub 验证身份并决定设备访问权限，Desktop 不解析该字段。发布顺序为官网、Hub、Desktop；不按设备名称合并或删除历史记录。
