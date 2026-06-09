# Tunnel Hub

Tunnel Hub 是一个自托管的 HTTP/WebSocket 反向隧道服务，默认托管域名为 `tunnel-hub.zenmind.cc`。它的目标是提供类似 Cloudflare Tunnel 的基础能力：本地 Agent 主动连接公网 Relay，公网请求按域名路由到内网服务。

项目包含：

- `zenmind-tunnel-server`: Go Relay 与 Agent。Relay 暴露公网入口、隧道 WebSocket、管理 API，并可托管构建后的前端；Agent 运行在内网环境，转发请求到本地服务。
- `zenmind-tunnel-website`: React + Vite 管理控制台，用于登录、管理路由、创建 Agent Token、查看 Agent 会话、事件和指标。
- `deploy`: Nginx/Caddy 示例配置。
- 根目录 `Dockerfile` 与 `docker-compose.yml`: 构建包含前端静态文件的 Relay 镜像，并用 SQLite volume 持久化数据。

## 工作方式

1. 公网用户访问 `auditor.tunnel-hub.zenmind.cc`。
2. Relay 根据请求 Host 查询已启用路由，例如 `auditor.tunnel-hub.zenmind.cc -> http://127.0.0.1:3000`。
3. Relay 通过 `/tunnel` 上已连接的 Agent 打开 yamux stream。
4. Agent 在内网侧请求目标服务，并把响应通过同一 stream 返回 Relay。
5. WebSocket 请求会升级为双向帧转发。

当前实现一次只保留一个活跃 Agent 会话；新 Agent 连接会替换旧连接。

## 快速开始

### 1. 启动 Relay

```bash
cd zenmind-tunnel-server
RELAY_ADDR=:8080 \
BOOTSTRAP_ADMIN_USERNAME=admin \
BOOTSTRAP_ADMIN_PASSWORD=admin \
COOKIE_SECRET=local-development-cookie-secret \
go run ./cmd/relay
```

Relay 默认监听 `:8080`，使用当前目录下的 `zenmind-tunnel.db`。

### 2. 启动管理前端

```bash
cd zenmind-tunnel-website
npm install
npm run dev
```

Vite 开发服务器会把同源外的请求发到页面配置的 API 地址。默认 `VITE_API_BASE_URL` 为空，适合前端和 Relay 同源部署；本地开发可在启动前设置：

```bash
VITE_API_BASE_URL=http://127.0.0.1:8080 npm run dev
```

使用 `admin/admin` 或你在 Relay 环境变量中设置的账号密码登录。

### 3. 创建 Token 并启动 Agent

在管理前端创建 Tunnel Token，复制只展示一次的 secret，然后运行：

```bash
cd zenmind-tunnel-server
AGENT_TOKEN=<token-secret> \
AGENT_RELAY_URL=ws://127.0.0.1:8080/tunnel \
go run ./cmd/agent
```

生产环境请使用 `wss://<relay-host>/tunnel`。

### 4. 创建路由

在管理前端添加路由：

```text
auditor.tunnel-hub.zenmind.cc -> http://127.0.0.1:3000
dashboard.tunnel-hub.zenmind.cc -> http://127.0.0.1:8080
```

请求到 Relay 的 Host 必须和 `publicHost` 匹配。TLS 通常由 Nginx、Caddy、ALB 或其他反向代理终止，再转发到 Relay。

也可以为自动化脚本创建 Admin API Key，然后用托管服务接口发布本机服务：

```bash
curl -X PUT https://admin.tunnel-hub.zenmind.cc/api/admin/services/auditor \
  -H "Authorization: Bearer $TUNNEL_HUB_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:3000","active":true}'
```

该接口会固定生成 `auditor.tunnel-hub.zenmind.cc`，服务名必须是单个小写 DNS label。

## 本地测试

后端：

```bash
cd zenmind-tunnel-server
go test ./...
```

前端：

```bash
cd zenmind-tunnel-website
npm test
npm run build
```

## Docker

根目录 Dockerfile 会先构建前端，再构建 Go 二进制，最终镜像默认运行 `/app/relay`，并把前端静态文件放在 `/app/website`。

```bash
docker compose up --build
```

默认 compose 配置：

- Relay 监听 `8080:8080`
- SQLite 存在 volume `relay-data` 的 `/data/zenmind-tunnel.db`
- 管理站点 Host 为 `admin.localhost`
- 前端静态目录为 `/app/website`

浏览器访问管理站点时需要带上匹配 Host，例如：

```bash
curl -H 'Host: admin.localhost' http://127.0.0.1:8080/
```

## Relay 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `RELAY_ADDR` | `:8080` | Relay 监听地址。 |
| `RELAY_DB_PATH` | `zenmind-tunnel.db` | SQLite 数据库路径。 |
| `ADMIN_HOST` | 空 | 管理前端域名，例如 `admin.example.com`。设置后 Relay 会在该 Host 上托管 `WEBSITE_DIST`。 |
| `WEBSITE_DIST` | 空 | 构建后的前端目录，例如 `/app/website` 或 `zenmind-tunnel-website/dist`。 |
| `PUBLIC_BASE_DOMAIN` | `tunnel-hub.zenmind.cc` | 托管服务接口使用的根域名，例如 `auditor.tunnel-hub.zenmind.cc`。 |
| `COOKIE_SECRET` | 随机开发值 | 管理后台 session 签名密钥。生产环境必须设置为稳定的长随机字符串。 |
| `BOOTSTRAP_ADMIN_USERNAME` | `admin` | 首次启动时创建的管理员用户名。数据库已有管理员后不会覆盖。 |
| `BOOTSTRAP_ADMIN_PASSWORD` | `admin` | 首次启动时创建的管理员密码。生产环境必须修改。 |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | HTTP 请求体最大缓冲字节数，默认 64 MiB。 |

## Agent 环境变量

| 名称 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_TOKEN` | 必填 | 管理后台/API 创建的 Token secret。 |
| `AGENT_RELAY_URL` | `ws://127.0.0.1:8080/tunnel` | Relay 隧道地址。生产环境通常为 `wss://.../tunnel`。 |
| `AGENT_TLS_INSECURE_SKIP_VERIFY` | `false` | 跳过 TLS 校验，仅用于开发或临时排障。 |
| `AGENT_RECONNECT_SECONDS` | `3` | Agent 断线后的重连间隔。 |

## 管理 API

管理 API 前缀为 `/api/admin`。控制台登录后使用 HttpOnly cookie `zenmind_admin` 鉴权；自动化脚本可使用 Admin API Key 通过 `Authorization: Bearer <key>` 鉴权。本地开发 CORS 允许 `localhost` 与 `127.0.0.1` 来源携带 cookie。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/admin/login` | 登录，body: `{ "username": "...", "password": "..." }`。 |
| `POST` | `/api/admin/logout` | 登出。 |
| `GET` | `/api/admin/me` | 当前用户。 |
| `GET` | `/api/admin/routes` | 列出路由。 |
| `POST` | `/api/admin/routes` | 创建路由，body: `{ "publicHost": "...", "targetUrl": "...", "active": true }`。 |
| `PUT` | `/api/admin/routes/{id}` | 更新路由。 |
| `DELETE` | `/api/admin/routes/{id}` | 删除路由。 |
| `GET` | `/api/admin/api-keys` | 列出 Admin API Key 元数据。 |
| `POST` | `/api/admin/api-keys` | 创建 Admin API Key，body: `{ "name": "..." }`，响应中的 `secret` 只返回一次。 |
| `DELETE` | `/api/admin/api-keys/{id}` | 停用 Admin API Key。 |
| `GET` | `/api/admin/services/{name}` | 查询托管服务路由，例如 `auditor`。 |
| `PUT` | `/api/admin/services/{name}` | 创建或更新托管服务，body: `{ "targetUrl": "...", "active": true }`。 |
| `DELETE` | `/api/admin/services/{name}` | 删除托管服务路由。 |
| `GET` | `/api/admin/tokens` | 列出 Token 元数据。 |
| `POST` | `/api/admin/tokens` | 创建 Token，body: `{ "name": "..." }`，响应中的 `secret` 只返回一次。 |
| `DELETE` | `/api/admin/tokens/{id}` | 停用 Token。 |
| `GET` | `/api/admin/sessions` | 最近 Agent 会话。 |
| `GET` | `/api/admin/events` | 最近事件。 |
| `GET` | `/api/admin/metrics` | 活跃 Agent 与 stream 指标。 |

## 部署提示

- 将 `COOKIE_SECRET`、`BOOTSTRAP_ADMIN_PASSWORD` 换成强随机值；首次启动完成后请妥善保存管理员凭据。
- Relay 应部署在 TLS 终止层之后，并保留 `Host`、`Upgrade`、`Connection`、`X-Forwarded-*` 等头。
- 所有需要公开的业务域名都应指向同一个 Relay；Relay 使用 Host 查找 route。
- 为托管服务配置 wildcard DNS，例如 `*.tunnel-hub.zenmind.cc` 指向 Relay 入口。
- TLS 终止层需要覆盖 `admin.tunnel-hub.zenmind.cc` 和 `*.tunnel-hub.zenmind.cc`，并保留原始 `Host`、`Upgrade`、`Connection`、`X-Forwarded-*` 等头。
- `ADMIN_HOST` 应使用独立管理域名，例如 `admin.tunnel-hub.zenmind.cc`，不建议和业务路由域名混用。
- 生产数据库建议挂载持久化目录，避免容器重建丢失 routes、tokens、sessions 和 events。
- `deploy/nginx/zenmind-tunnel.conf` 与 `deploy/caddy/Caddyfile` 是示例，需要替换域名和证书路径。

## 目录速览

```text
.
├── Dockerfile
├── docker-compose.yml
├── deploy/
│   ├── caddy/Caddyfile
│   └── nginx/zenmind-tunnel.conf
├── zenmind-tunnel-server/
│   ├── cmd/relay
│   ├── cmd/agent
│   ├── internal/admin
│   ├── internal/auth
│   ├── internal/config
│   ├── internal/proxy
│   ├── internal/store
│   └── internal/tunnel
└── zenmind-tunnel-website/
    ├── src/App.tsx
    ├── src/lib/api.ts
    └── src/styles.css
```
# tunnel-hub-server
# tunnel-hub-server
# tunnel-hub-server
