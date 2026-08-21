# AGENTS.md

本文件给后续在 `tunnel-hub-server` 中工作的编码代理和开发者使用。请先读 `README.md`，再按本文件约定改动。

## 1. 项目概览

`tunnel-hub-server` 是 Tunnel Hub 的 Go 后端，核心边界是 Relay、Agent/Desktop 出站连接、管理 API、Desktop 注册 API、SQLite 持久化和公网 HTTP/WebSocket 转发。

管理前端在 sibling 项目 `tunnel-hub-website`，Desktop 协议调试台在 sibling 项目 `tunnel-hub-tester`。Desktop public mini site 是本仓库内的独立子项目 `tunnel-hub-public/`，作为单独静态容器部署；不要把管理后台 React/Vite 前端重新放回本项目。

## 2. 技术栈

- 语言/runtime: Go 1.26。
- HTTP: 标准库 `net/http`。
- WebSocket: `github.com/gorilla/websocket`。
- 复用连接: `github.com/hashicorp/yamux`，通过 `replace` 指向本地 `third_party/yamux`。
- 存储: SQLite，驱动为 `modernc.org/sqlite`，不依赖 CGO。
- 鉴权: 本地 admin session cookie + 官网 SSO JWT bearer token。
- 配置: `configs/brand.yaml` 是唯一品牌来源；`.env` 只承载非品牌运行参数。
- 部署: Docker multi-stage build，distroless runtime，Nginx/Caddy 负责公网 TLS 和路由。

## 3. 架构设计

Relay 入口在 `cmd/relay/main.go`，启动顺序是：

1. `internal/config` 加载 `.env`、严格校验品牌 YAML，再读取非品牌环境变量。
2. `internal/store` 打开 SQLite 并执行 schema/migration。
3. 可选根据 `ADMIN_USERNAME`/`ADMIN_PASSWORD` bootstrap 本地管理员。
4. 创建 `proxy.Manager` 管理在线 Agent/ Desktop tunnel session。
5. 挂载 Admin API、Desktop API、public component API、`/tunnel` 和公网 Host 转发。

主要流量链路：

- `/api/admin/*`: 管理 API。支持本地 `tunnel_hub_session` cookie，也支持官网 SSO JWT；JWT 必须满足 `role=admin` 且 `scope` 包含 `tunnel`。
- `/api/desktop/*`: Desktop 注册 API。只接受官网 SSO JWT，要求 `scope` 包含 `tunnel`。
- `/tunnel`: Agent 或 Desktop 主动连入的 WebSocket。旧 Agent 使用 `Authorization: Bearer <token>`；Desktop 推荐首帧发送 `ns=d` 的 `tunnel.open`。
- 普通服务 Host: 通过 `routes.public_host` 找 active route，打开对应 token 的 yamux stream，转发 HTTP/WebSocket 到 Agent 本地服务。
- `*.m.example.test`: Desktop public Host。WebSocket upgrade 请求进入 Relay，向 Desktop tunnel stream 发送 `ns=d` / `desktop.websocket.open` 元数据；普通 HTTP 由宿主机反向代理转发到 `tunnel-hub-public`。
- `*.m.example.test/api/upload`: Mobile 上传入口，只从请求 Host 确定 Desktop，内部发送 `ns=ap`, `type=/api/upload`；multipart 不允许携带 `publicHost`。
- `*.m.example.test/api/resource`: Mobile 资源入口，内部发送 `ns=ap`, `type=/api/resource` 和 `{file,pushURL}`；Desktop 通过 ticket 保护的 `/api/push/{id}` 回推文件。
- `*.wa.example.test`: Desktop WebApp public HTTP/WebSocket。Relay 通过 route 和 token 打开 Desktop stream，向 Desktop 发送 `ns=wa` 的 `http.request` 或 `websocket.connect` 元数据。
- `share.example.test`: 对话分享只读站点。公开边缘网关将 `/share/{id}` 和 `/assets/conversation-export/*` 转发到 Relay；Tunnel API origin 也必须暴露相同资产路径，供动态 origin 的本地和落盘导出使用。前者以一次 SQLite 主键查询读取有效 HTML 并原字节返回，后者从编译期 `embed.FS` 返回 WebClient 内容寻址资产。

## 4. 目录结构

- `cmd/relay`: Relay 进程入口，负责 API 挂载、数据库初始化、静态站点兼容托管和公网转发分派。
- `cmd/agent`: 通用 Agent 进程入口，连接 Relay 并转发到本地 HTTP/WebSocket 服务。
- `internal/admin`: 管理 API、本地登录、SSO JWT 管理鉴权、overview/activity/metrics 聚合、公开 component 列表。
- `internal/auth`: secret hash、admin password、SSO JWT 验证。
- `internal/config`: Relay/Agent 环境变量配置和 `.env` 加载。
- `configs`: Relay 与 Public 构建共用的品牌 YAML、可运行示例和跨实现测试 fixture。
- `internal/desktop`: Desktop device 和 Desktop WebApp 注册 API。
- `internal/proxy`: Relay/Agent 转发实现、yamux session、active agent manager、traffic event 记录。
- `internal/shareassets`: 公开对话显示资产的只读 handler 与编译期文件；目录按 asset-set hash 追加，不覆盖或删除已发布集合。
- `internal/store`: SQLite schema、migration、DAO 和领域模型。
- `internal/tunnel`: 隧道协议结构、JSON frame、WebSocket frame、Host/path/upstream 工具。
- `deploy`: Nginx/Caddy 示例配置。
- `third_party/yamux`: 本地替换的 yamux 依赖，除非明确修复复用协议问题，不要随意改。

## 5. 数据结构

核心表由 `internal/store/store.go` 的 `schema` 定义：

- `admin_users`: 本地管理用户。
- `admin_sessions`: 本地管理登录 session，cookie 名为 `tunnel_hub_session`。
- `tunnel_tokens`: Agent/Desktop tunnel token，仅存 hash 和 prefix。
- `routes`: public Host 到 target/token 的映射。
- `desktop_devices`: 用户维度的 Desktop 设备、随机 public Host、token 绑定。
- `desktop_webapps`: Desktop 设备下的 WebApp，绑定独立随机 `*.wa` Host 和 route。
- `agent_sessions`: Agent/Desktop tunnel 在线历史。
- `events`: 管理操作和系统事件。
- `traffic_events`: Desktop/WebApp/普通 route 的访问统计。
- `conversation_shares`: 用户创建的版本化不透明 HTML、会话关联、到期时间和撤销状态；公开 ID 必须不可预测，生成时使用 `share_` 前缀，但接收端只按 URL-safe 不透明 ID 校验。所有者身份来自官网 SSO JWT；Desktop main 是创建、列表、撤销的直接调用方，HTML 由 Agent Platform 预先生成。Relay 只校验传输契约并原字节保存/返回，不解析 HTML，也不保留旧事件流双协议。
- `conversation_share_access`: 每条分享最近一次成功公开访问时间；热更新与 HTML BLOB 分表，不保存访问日志或次数。

注意：`admin_api_keys` 仍在 schema 中，但当前主 API 路径没有完整使用它，不要把它当成已上线能力写入 README。

## 6. API 与协议定义

主要 HTTP API：

- `POST /api/admin/login`, `POST /api/admin/logout`, `GET /api/admin/me`
- `/api/admin/routes`
- `/api/admin/services/{name}`
- `/api/admin/tokens`
- `/api/admin/users`
- `GET /api/admin/overview`
- `GET /api/admin/desktops`
- `GET /api/admin/webapps`
- `GET /api/admin/activity`
- `GET /api/admin/agents`
- `GET /api/admin/sessions`
- `POST /api/admin/sessions/{id}/close`
- `GET /api/admin/events`
- `GET /api/admin/metrics`
- `GET /api/components`
- `POST /api/desktop/devices/register`
- `PUT /api/desktop/devices/{deviceId}/webapps/{name}`
- `POST /api/desktop/shares`, `GET /api/desktop/shares?conversationId=...`, `DELETE /api/desktop/shares/{shareId}`
- `GET /share/{shareId}`
- `GET/HEAD /assets/conversation-export/{assetSet}/{file}`
- `POST https://<desktop>.m.example.test/api/upload`
- `GET https://<desktop>.m.example.test/api/resource?file=<chat-relative-path>`

隧道协议要点：

- JSON metadata 使用 `internal/tunnel.WriteJSON` / `ReadJSON`，4 字节大端长度前缀，最大 `1 MiB`。
- WebSocket 数据帧使用 `internal/tunnel` 的 9 字节 frame envelope。
- Desktop tunnel open 首帧必须是 `v=1`, `ns=d`, `frame=request`, `type=tunnel.open`。
- WebApp HTTP metadata 使用 `ns=wa`, `type=http.request`。
- WebApp WebSocket metadata 使用 `ns=wa`, `type=websocket.connect`。
- Desktop public WebSocket metadata 使用 `ns=d`, `type=desktop.websocket.open`。
- Agent Platform 业务帧的 `ns=ap` 只存在于内部 WebSocket 协议，不映射成 HTTP URL 前缀。
- `/api/pull/{id}` 与 `/api/push/{id}` 是 ticket 保护的内部附件数据面，不属于客户端公共 API。

## 7. 开发要点

- 文档、配置和测试里的 API 路径必须以 `cmd/relay/main.go`、`internal/admin/server.go`、`internal/desktop/server.go` 为准。
- 品牌字段事实以 `configs/brand.yaml` 和 `internal/config/brand.go` 为准；环境变量事实以 `internal/config/config.go` 和 `.env.example` 为准。不要恢复旧品牌环境变量或写入真实生产值。
- `.env.example` 的 SSO issuer 默认留空，并预留 public key file；需要调试 SSO 或 Desktop 注册时，必须同时配置 issuer 和有效 `configs/jwt-public.pem`。
- Host 匹配必须统一经过 `internal/tunnel.NormalizeHost` 或等价逻辑，避免大小写、端口、尾点导致 route 绕过。
- `proxy.Manager` 以 token 维护在线 Agent；同一 token 新连接会替换旧连接。新增功能时要考虑 token/session 的一对多和替换行为。
- HTTP 请求体当前在 Relay 侧完整缓冲，限制由 `MAX_REQUEST_BODY_BYTES` 控制。涉及大文件、流式上传或 backpressure 的改动要重点测试。
- Desktop public Host 不使用 `deviceId`，由随机 label 加 `domains.desktopPublicBase` 生成；WebApp Host 由随机 label 加 `domains.webAppPublicBase` 生成。
- Desktop/platform auth token 由 Desktop 侧校验，Relay 只负责把 query token 或 `bearer.<token>` subprotocol 透传给 Desktop。
- 附件 API 的 Desktop 身份只来自 `<desktop>.m.example.test` Host；不得从 body、query 或其他客户端字段接受 `publicHost` 覆盖。
- 管理 token 手动创建当前禁用；Desktop 注册会创建/轮换 tunnel token。
- 对话显示资产必须由 Agent WebClient 同一次构建同步，路径中的 64 位 hash 是不可变集合身份；handler 只接受白名单路径并返回长期 immutable、CORS/CORP 与 `nosniff` 响应头。永久分享存在时不得清理历史集合。
- Go 改动提交前运行 `gofmt -w` 和相关 `go test`。
- React/Vite 改动同时运行 `tunnel-hub-public` 的 `npm test`，并用非空品牌文件执行 `npm run build`；测试模式默认读取 `brand.example.yaml`。

## 8. 开发流程

后端本地验证：

```bash
cd tunnel-hub-server
go test ./...
go run ./cmd/relay
```

Agent 联调：

```bash
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:8080/tunnel go run ./cmd/agent
```

管理前端联调：

```bash
cd ../tunnel-hub-website
npm install
npm test
npm run build
npm run dev
```

Desktop 协议联调：

```bash
cd ../tunnel-hub-tester
npm install
npm test
npm run build
npm run dev
```

## 9. 已知约束与注意事项

- 根目录当前是三个 sibling 项目，不是一个统一 git 根目录；不要假设可以在上级目录使用 git 历史或提交。
- `.env`、SQLite 数据库、JWT key、真实 token、`configs/*.pem` 都不能提交。
- 生产部署依赖反向代理正确转发 WebSocket upgrade；修改部署文档时要同时检查 wildcard Host 路由。
- `*.m.example.test` 的 WebSocket upgrade 必须继续直达 Relay；普通 HTTP 在生产反向代理层应转到 `tunnel-hub-public`。如果普通 HTTP 到达 Relay，Relay 仍会返回 upgrade required。
- `*.wa.example.test` 是 browser-facing WebApp 代理，不等同于 tester 中的 Desktop business namespace `ns=wa`。
- `third_party/yamux` 是本地替换依赖，改动需要说明原因并跑完整隧道测试。
- 如果环境限制导致无法运行验证命令，最终说明里必须明确列出未运行项和原因。
