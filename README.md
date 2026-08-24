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
- `*.wa.example.test`: Desktop WebApp 反向代理入口，支持 HTTP 和 WebSocket。
- `share.example.test`: 对话分享的公开只读 origin；边缘网关将 `/share/*` 和 `/assets/conversation-export/*` 转发到 Relay。前者返回已存储的轻量 HTML，后者返回 WebClient 构建的内容寻址显示资源。

WebApp 有两条独立链路：

- 手机配对访问使用 `https://<device>-<frontendPort>.m.example.test/`，不创建数据库 WebApp route。首次导航携带配对 `app` token，Relay 换成 HttpOnly Cookie 并重定向到无 token 地址；每个请求仍由 Desktop 校验 token scope、device id 和运行中的 WebApp 端口。
- 用户主动公开分享使用 `*.wa.example.test`。该路由仅由 Desktop 的“一键发布”注册和启停，保持匿名 URL 分享语义。

## 2. 快速开始

### 前置要求

- Go 1.26
- Docker / Docker Compose
- OpenSSL，可选，用于从官网 SSO 私钥导出 JWT 公钥
- 一个可用的官网 SSO JWT 公钥，生产和 Desktop 注册 API 必需

### 本地启动 Relay

```bash
cd tunnel-hub-server
cp .env.example .env
mkdir -p .local
cp configs/brand.example.yaml .local/brand.yaml
go test ./...
go run ./cmd/relay
```

复制 `.env.example` 后，把 `configs/brand.example.yaml` 复制到已忽略的 `.local/brand.yaml`，再显式填写 Relay 地址、数据库路径和 SSO 校验参数。仓库提交的 `brand.yaml` 故意保留完整空键，不能直接启动；`brand.example.yaml` 是可运行示例。

Relay 启动要求同时提供 SSO issuer、audience、用户 ID claim，以及文件或 PEM 形式的有效 JWT 公钥。缺少任何一项都会在监听端口前失败并指出字段。

如果需要启用本地管理账号，在 `.env` 中设置：

```bash
ADMIN_USERNAME=admin
ADMIN_PASSWORD=<local-password>
```

`ADMIN_PASSWORD` 为空时不会自动创建本地管理员。

### 启动 Agent

Agent 需要使用已创建的 tunnel token。Desktop 新注册后返回的是内部 `agentToken`；普通 Agent 可使用已有 active token。

```bash
cd tunnel-hub-server
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:11961/tunnel go run ./cmd/agent
```

生产环境使用：

```bash
AGENT_RELAY_URL=wss://hub.example.test/tunnel
```

### 本地容器运行

```bash
cd tunnel-hub-server
cp .env.example .env
mkdir -p .local
cp configs/brand.example.yaml .local/brand.yaml
docker compose up --build
```

`docker-compose.yml` 会把数据库写入命名卷，并分别只读挂载 `.local/brand.yaml` 与 `configs/jwt-public.pem`。启动前请确认两者均已准备好。

## 3. 配置说明

本项目会自动加载当前工作目录下的 `.env`。真实 shell 环境变量或容器环境变量优先级高于 `.env`。不要提交真实密钥、token、密码或生产 JWT key material。

### 品牌配置

Relay 和 `tunnel-hub-public` 构建共同读取 `BRAND_CONFIG_FILE` 选择的 YAML，字段契约如下：

| YAML 字段 | 必填 | 说明 |
| --- | --- | --- |
| `schemaVersion` | 是 | 当前固定为 `1`。 |
| `brand.id` | 是 | 稳定部署身份，只允许小写字母开头以及小写字母、数字、连字符。该值派生 mobile session Cookie，投产后不要随意修改。 |
| `brand.productName` | 是 | 产品显示名。 |
| `brand.publicSiteTitle` | 是 | Public 页面构建时注入的完整标题。 |
| `domains.publicBase` | 是 | Tunnel API/管理入口 hostname。 |
| `domains.desktopPublicBase` | 是 | Desktop public wildcard 根 hostname。 |
| `domains.webAppPublicBase` | 是 | WebApp wildcard 根 hostname。 |
| `endpoints.relayPublicUrl` | 是 | Relay WebSocket URL；非 Loopback 地址必须使用 `wss` 和 `/tunnel`。 |
| `endpoints.sharePublicBaseUrl` | 是 | 公开分享 origin；非 Loopback 地址必须使用 HTTPS，本地 Loopback 可使用 HTTP。 |

三个 domain 只接受互不相同的 hostname，不接受 scheme、端口、路径、通配符或 IP。Go 与 Vite 都严格拒绝未知字段、错误类型、多文档和非法值。`BRAND_CONFIG_FILE` 仅选择 YAML 文件，不覆盖其中字段；本地真实文件放在已忽略的 `.local/` 下。

HTTPS 下 mobile session Cookie 为 `__Host-<brand.id>_mobile_session`，本地 HTTP 下为 `<brand.id>_mobile_session`。

### Relay 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `BRAND_CONFIG_FILE` | 必填 | 品牌 YAML 路径；本地真实文件应放在 `.local/`，容器中固定为 `/configs/brand.yaml`。 |
| `RELAY_ADDR` | 必填 | Relay 本地监听地址；示例为 `:11961`，容器内由 Compose 覆盖为 `:8080`。 |
| `RELAY_DB_PATH` | 必填 | SQLite 数据库路径；示例为 `tunnel.db`，容器中通常设置为 `/data/tunnel.db`。 |
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
| `SSO_JWT_ALLOW_MISSING_TUNNEL_SCOPE` | `false` | 兼容开关；为 `true` 时管理和 Desktop 注册不再要求 `scope=tunnel`。 |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | Relay 缓冲 HTTP 请求体的最大字节数。 |
| `TRUSTED_PROXY_CIDRS` | 空 | 可信反向代理 CIDR，命中后才读取 `X-Real-IP` / `X-Forwarded-For`；生产 Docker + nginx 建议 `172.23.0.1/32,127.0.0.1/32,::1/128`。 |

### Agent 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_RELAY_URL` | `ws://127.0.0.1:11961/tunnel` | Agent 默认 Relay tunnel WebSocket 地址。 |
| `AGENT_TOKEN` | 必填 | Agent/desktop tunnel token。 |
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
mkdir -p bin
go build -o ./bin/relay ./cmd/relay
go build -o ./bin/agent ./cmd/agent
```

### 构建镜像

```bash
cd tunnel-hub-server
docker build -t tunnel-hub-server:latest .
docker build -f tunnel-hub-public/Dockerfile --build-arg BRAND_CONFIG_FILE=configs/brand.example.yaml -t tunnel-hub-public:example .
```

镜像会同时构建 `/app/relay` 和 `/app/agent`，默认入口是 `/app/relay`。

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
- `*.wa.example.test`: 直接转发到 Relay。
- `share.example.test/share/*` 由公开边缘网关直接转发到 Relay。Relay 查询 SQLite 后返回 HTML；`/assets/conversation-export/*` 在分享 origin 和 Tunnel API origin 都返回编入 Relay 的不可变 JS/CSS/font 资产，供线上分享、落盘导出和本地 loopback 环境复用。HTML 资源 origin 由 Desktop Worker 按当前 Tunnel 配置注入，不绑定固定域名。

Tunnel 端模板在 `deploy/nginx/tunnel-hub.conf.template` 和 `deploy/caddy/Caddyfile.template`，其中包含分享 origin 的 `/share/*`，以及分享/Tunnel API 两个 origin 的 `/assets/conversation-export/*` 直连 Relay 规则。上线前必须替换全部 `{{...}}` 占位符；分享关闭时删除分享 Host block。

| 模板占位符 | YAML 来源 |
| --- | --- |
| `{{PUBLIC_BASE}}` | `domains.publicBase` |
| `{{DESKTOP_PUBLIC_BASE}}` | `domains.desktopPublicBase` |
| `{{WEBAPP_PUBLIC_BASE}}` | `domains.webAppPublicBase` |
| `{{SHARE_HOST}}` | `endpoints.sharePublicBaseUrl` 的 hostname |
| `{{DESKTOP_PUBLIC_BASE_REGEX}}` | 转义正则元字符后的 `domains.desktopPublicBase` |
| `{{ACME_EMAIL}}`、`{{*_CERTIFICATE}}`、`{{*_CERTIFICATE_KEY}}` | 部署环境的 ACME 联系邮箱和证书绝对路径，不来自品牌 YAML |

## 5. 运维

### 常用检查

```bash
go test ./...
docker compose ps
docker logs tunnel-hub-server
```

### 数据与备份

- SQLite 数据库路径由 `RELAY_DB_PATH` 控制。
- 容器部署默认数据库在 Docker volume `tunnel-hub-server_relay-data`。
- 备份前建议停止写入流量，或使用 SQLite 安全备份方式复制数据库。

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
  -d '{"deviceId":"mac-mini","deviceName":"Frank MacBook Pro","rotateToken":false}'
```

创建、读取和撤销对话分享：

Relay 把 Desktop 常驻 Worker 已渲染、由 Desktop main 原样转发的完整 HTML 当作不透明字节保存，不解析 DOM、标题、消息或事件。正文最大 20 MiB，必须是非空 UTF-8。创建请求必须同时提供 `X-Conversation-Document-Version: 1`、非空 `X-Conversation-ID` 和 `X-Conversation-Share-Expiration`；时效只接受 `5m`、`30m`、`1h`、`3h`、`1d`、`5d`、`15d`、`30d`、`permanent`。任一 Header 缺失或非法都会在读取正文前返回 400。生产调用方是 Desktop main：Worker 从 Platform 请求 Snapshot、从 WebClient 请求模板，生成后由 main 创建、列表和撤销；Platform 不接收 Tunnel token，也不感知模板或分享生命周期。下面命令只用于服务端联调。

```bash
curl -X POST https://hub.example.test/api/desktop/shares \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT" \
  -H "Content-Type: text/html; charset=utf-8" \
  -H "X-Conversation-Document-Version: 1" \
  -H "X-Conversation-ID: chat_xxx" \
  -H "X-Conversation-Share-Expiration: 30d" \
  --data-binary @conversation.html

curl 'https://hub.example.test/api/desktop/shares?conversationId=chat_xxx' \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT"

curl https://share.example.test/share/share_xxx

curl -I https://share.example.test/assets/conversation-export/<asset-set-hash>/runtime.js

curl -X DELETE https://hub.example.test/api/desktop/shares/share_xxx \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT"
```

创建和列表响应的 `createdAt`、有限 `expiresAt` 与非空 `lastAccessedAt` 使用 RFC3339；永久 `expiresAt` 和尚未访问的 `lastAccessedAt` 明确返回 JSON `null`。列表只返回当前所有者、指定会话下未撤销且未到期的元数据，不读取 HTML。匿名 `GET /share/{id}` 只返回仍有效且未撤销的原始 HTML bytes，媒体类型为 `text/html; charset=utf-8`，并设置 `no-store`、`nosniff`、`noindex` 与 `no-referrer`；成功 GET 会 best-effort 更新独立访问元数据行，写入失败不影响正文。撤销、到期和未知 ID 统一返回最小 404 HTML；永久链接也可由所有者撤销。

`GET/HEAD /assets/conversation-export/{sha256}/{file}` 只提供随 Relay 编译的白名单资产，响应使用精确 MIME、`nosniff`、跨 origin 读取许可和一年 `immutable` 缓存。资产目录是追加式发布：已经被模板引用的 hash 不允许覆盖或删除，分享撤销也不删除公共显示资源。

注册 Desktop WebApp：

```bash
curl -X PUT https://hub.example.test/api/desktop/devices/mac-mini/webapps/notes \
  -H "Authorization: Bearer $OFFICIAL_SSO_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:5173","active":true}'
```

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
- `desktop is offline` / `assigned desktop is offline`: 确认 Desktop 或 Agent 已连接 `/tunnel`，且 token 仍为 active。
- WebSocket 无法升级：检查反向代理是否保留 `Upgrade` 和 `Connection` 头。
- Desktop public mini site 没有打开：确认 `*.m.example.test` 普通 HTTP 已转发到 `tunnel-hub-public`，不是 Relay。
- 附件上传返回 `desktop is offline`：确认请求 Host 对应的 Desktop 已连接 `/tunnel`。
- 附件资源返回 `desktop resource timed out`：确认 Desktop 已实现 `/api/resource` 业务帧，并能访问 Hub 提供的 ticket 保护回推 URL。
- 公网 Host 404：检查 DNS wildcard、Nginx/Caddy wildcard route，以及 `brand.yaml` 的三个 `domains` 字段。

### 跨应用发布检查

- Desktop 注册响应字段 `relayUrl`、`publicHost`、`publicUrl`、`webSocketUrl` 保持不变；Desktop 与移动 WebApp 继续动态消费这些值。
- 发布前核对对应环境仓库的 `tunnelHub.relayUrl` 与 `brand.yaml.endpoints.relayPublicUrl` 完全一致，本仓库不修改 sibling 应用。
- `tunnel-hub-tester` 的远程附件 helper 仍限制在其既有域名，非当前品牌环境只保证可手工填写 URL 做普通 WebSocket 调试，附件适配另行处理。
- 顺序为：准备非空品牌 YAML，用同一文件构建 public 和渲染代理模板，最后同时发布 Relay、public 与代理配置。分享表直接使用当前 schema；未发布的本地调试数据库需由开发者删除后重建。
- HTTP 上传失败：检查 `MAX_REQUEST_BODY_BYTES`，当前 Relay 会完整缓冲请求体。

## 6. 开发命令

```bash
go test ./...
go test ./internal/proxy -run Test
go test ./internal/admin -run Test
gofmt -w ./cmd ./internal
```

提交前至少运行 `go test ./...`。协议、转发、鉴权、配置、存储相关改动需要补充或更新对应 `*_test.go`。
