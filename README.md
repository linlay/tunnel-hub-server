# Tunnel Hub Server

## 1. 项目简介

`tunnel-hub-server` 是 Tunnel Hub 的 Go 后端，负责公网 Relay、Agent/Zenmind Desktop 出站隧道、管理 API、Desktop 注册 API、公开组件列表，以及基于 Host 的 HTTP/WebSocket 转发。

当前生产形态是拆分部署：

- `tunnel-hub-server`: 后端 Relay 和 API。
- `tunnel-hub-website`: React/Vite 管理前端，作为独立静态站点容器部署。
- `tunnel-hub-public`: Desktop public Host 的轻量浏览器客户端，作为独立静态站点容器部署。
- `tunnel-hub-tester`: 本地 Desktop WebSocket 调试台，不参与生产流量。

典型域名规划：

- `tunnel-hub.zenmind.cc`: 管理前端、`/api/admin`、`/api/desktop`、`/api/components`、`/api/upload`、`/api/pull`、`/tunnel`。
- `*.m.zenmind.cc`: 普通浏览器请求打开 Desktop public mini site；WebSocket upgrade、`/api/upload` 和 `/api/pull` 请求进入 Relay。
- `*.wa.zenmind.cc`: Desktop WebApp 反向代理入口，支持 HTTP 和 WebSocket。

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
mkdir -p configs
go test ./...
go run ./cmd/relay
```

Relay 默认监听 `:8080`，本地 SQLite 数据库默认是 `tunnel.db`。

`.env.example` 默认启用了官网 SSO JWT issuer 和 `configs/jwt-public.pem` 路径。只做本地管理账号调试时，可以把 `.env` 里的 `SSO_JWT_ISSUER`、`SSO_JWT_PUBLIC_KEY_FILE`、`SSO_JWT_PUBLIC_KEY_PEM` 置空；需要调试官网 SSO 或 Desktop 注册 API 时，必须先准备有效 JWT 公钥。

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
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:8080/tunnel go run ./cmd/agent
```

生产环境使用：

```bash
AGENT_RELAY_URL=wss://tunnel-hub.zenmind.cc/tunnel
```

### 本地容器运行

```bash
cd tunnel-hub-server
cp .env.example .env
mkdir -p configs
docker compose up --build
```

`docker-compose.yml` 会把数据库写入命名卷，并把本地 `./configs` 只读挂载到容器 `/configs`。如果 `.env` 中保留了 `SSO_JWT_PUBLIC_KEY_FILE`，请确认 `configs/jwt-public.pem` 已存在。

## 3. 配置说明

本项目会自动加载当前工作目录下的 `.env`。真实 shell 环境变量或容器环境变量优先级高于 `.env`。不要提交真实密钥、token、密码或生产 JWT key material。

### Relay 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `RELAY_ADDR` | `:8080` | Relay 监听地址。 |
| `RELAY_DB_PATH` | `tunnel.db` | SQLite 数据库路径；容器中通常设置为 `/data/tunnel.db`。 |
| `ADMIN_HOST` | 空 | 旧版 Relay 静态管理站点 Host；拆分部署时保持为空。 |
| `WEBSITE_DIST` | 空 | 旧版 Relay 静态站点目录；拆分部署时保持为空。 |
| `PUBLIC_BASE_DOMAIN` | `tunnel-hub.zenmind.cc` | `/api/admin/services/{name}` 创建服务 Host 时使用。 |
| `DESKTOP_PUBLIC_BASE_DOMAIN` | `m.zenmind.cc` | Desktop public WebSocket 随机 Host 的根域。 |
| `WEBAPP_PUBLIC_BASE_DOMAIN` | `wa.zenmind.cc` | Desktop WebApp public Host 的根域。 |
| `ADMIN_USERNAME` | `admin` | 本地管理账号 bootstrap 用户名。 |
| `ADMIN_PASSWORD` | 空 | 本地管理账号 bootstrap 密码；为空时跳过创建。 |
| `ADMIN_SESSION_TTL` | `24h` | 本地管理登录 cookie 有效期。 |
| `COOKIE_SECURE` | `false` | 管理 cookie 是否只允许 HTTPS。生产 HTTPS 下建议设为 `true`。 |
| `SSO_JWT_ISSUER` | 空 | 官网 SSO JWT issuer，生产和 Desktop 注册 API 必填。 |
| `SSO_JWT_PUBLIC_KEY_FILE` | 空 | 官网 SSO JWT PEM 公钥文件路径。 |
| `SSO_JWT_PUBLIC_KEY_PEM` | 空 | 官网 SSO JWT PEM 公钥内容，支持转义 `\n`。 |
| `SSO_JWT_AUDIENCE` | `zenmind-tunnel-hub-server` | JWT audience。 |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | Relay 缓冲 HTTP 请求体的最大字节数。 |
| `TRUSTED_PROXY_CIDRS` | 空 | 可信反向代理 CIDR，命中后才读取 `X-Real-IP` / `X-Forwarded-For`；生产 Docker + nginx 建议 `172.23.0.1/32,127.0.0.1/32,::1/128`。 |

### Agent 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_RELAY_URL` | `ws://127.0.0.1:8080/tunnel` | Relay tunnel WebSocket 地址。 |
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
```

镜像会同时构建 `/app/relay` 和 `/app/agent`，默认入口是 `/app/relay`。

### Docker Compose

```bash
cd tunnel-hub-server
docker compose up -d --build
```

默认映射 `8080:8080`。生产宿主机可以改为只监听内网端口，再由 Nginx/Caddy 终止 TLS 并转发。

### 拆分生产部署

推荐部署拓扑：

- Relay: `127.0.0.1:11961 -> 8080`
- Website: `127.0.0.1:11963 -> 80`
- Public Desktop site: `127.0.0.1:11965 -> 80`
- `tunnel-hub.zenmind.cc/`: 转发到 website 容器。
- `tunnel-hub.zenmind.cc/api/admin`, `/api/desktop`, `/api/components`, `/api/upload`, `/api/pull`, `/tunnel`: 转发到 Relay。
- `*.m.zenmind.cc`: WebSocket upgrade、`/api/upload` 和 `/api/pull` 转发到 Relay；普通 HTTP 转发到 public Desktop site。
- `*.wa.zenmind.cc`: 直接转发到 Relay。

示例配置在 `deploy/nginx/zenmind-tunnel.conf` 和 `deploy/caddy/Caddyfile`。

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
curl -i http://127.0.0.1:8080/api/admin/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<local-password>"}'
```

使用官网 SSO JWT 发布普通服务：

```bash
curl -X PUT https://tunnel-hub.zenmind.cc/api/admin/services/auditor \
  -H "Authorization: Bearer $ZENMIND_OFFICIAL_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:3000","tokenId":"token_...","active":true}'
```

注册 Desktop：

```bash
curl -X POST https://tunnel-hub.zenmind.cc/api/desktop/devices/register \
  -H "Authorization: Bearer $ZENMIND_OFFICIAL_JWT" \
  -H "Content-Type: application/json" \
  -d '{"deviceId":"mac-mini","deviceName":"Frank MacBook Pro","rotateToken":false}'
```

注册 Desktop WebApp：

```bash
curl -X PUT https://tunnel-hub.zenmind.cc/api/desktop/devices/mac-mini/webapps/notes \
  -H "Authorization: Bearer $ZENMIND_OFFICIAL_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:5173","active":true}'
```

上传附件到 Desktop chat：

```bash
curl -X POST https://tunnel-hub.zenmind.cc/api/upload \
  -H "Authorization: Bearer $DESKTOP_APP_TOKEN" \
  -F publicHost=zmxxxx.m.zenmind.cc \
  -F chatId=chat_xxx \
  -F file=@./note.txt
```

公开组件列表：

```bash
curl https://tunnel-hub.zenmind.cc/api/components
```

### 常见排查

- `official JWT verifier is not configured`: 检查 `SSO_JWT_ISSUER` 和 JWT 公钥配置。
- 启动时报 `configs/jwt-public.pem` 不存在：准备有效公钥，或在只做本地账号调试时清空 `.env` 里的 SSO JWT 相关变量。
- 管理台无法登录：确认 `ADMIN_PASSWORD` 首次启动时已设置，或使用官网 SSO JWT 调用 API。
- `desktop is offline` / `assigned desktop is offline`: 确认 Desktop 或 Agent 已连接 `/tunnel`，且 token 仍为 active。
- WebSocket 无法升级：检查反向代理是否保留 `Upgrade` 和 `Connection` 头。
- Desktop public mini site 没有打开：确认 `*.m.zenmind.cc` 普通 HTTP 已转发到 `tunnel-hub-public`，不是 Relay。
- 附件上传返回 `desktop is offline`：确认对应 `publicHost` 的 Desktop 已连接 `/tunnel`。
- 公网 Host 404：检查 DNS wildcard、Nginx/Caddy wildcard route、`PUBLIC_BASE_DOMAIN` / `DESKTOP_PUBLIC_BASE_DOMAIN` / `WEBAPP_PUBLIC_BASE_DOMAIN`。
- HTTP 上传失败：检查 `MAX_REQUEST_BODY_BYTES`，当前 Relay 会完整缓冲请求体。

## 6. 开发命令

```bash
go test ./...
go test ./internal/proxy -run Test
go test ./internal/admin -run Test
gofmt -w ./cmd ./internal
```

提交前至少运行 `go test ./...`。协议、转发、鉴权、配置、存储相关改动需要补充或更新对应 `*_test.go`。
