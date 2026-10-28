# AGENTS.md

本文件给后续在 `tunnel-hub-server` 中工作的编码代理和开发者使用。请先读 `README.md`，再按本文件约定改动。

## 1. 项目概览

`tunnel-hub-server` 是 Tunnel Hub 的 Go 后端，核心边界是 Relay、Agent/Desktop 出站连接、管理 API、Desktop 注册 API、MySQL/SQLite 持久化和公网 HTTP/WebSocket 转发。

管理前端在 sibling 项目 `tunnel-hub-website`，Desktop 协议调试台在 sibling 项目 `tunnel-hub-tester`。Desktop public mini site 是本仓库内的独立子项目 `tunnel-hub-public/`，作为单独静态容器部署；不要把管理后台 React/Vite 前端重新放回本项目。

## 2. 技术栈

- 语言/runtime: Go 1.25，所有构建入口固定 `GOTOOLCHAIN=local`。
- HTTP: 标准库 `net/http`。
- WebSocket: `github.com/gorilla/websocket`。
- 复用连接: `github.com/hashicorp/yamux`，通过 `replace` 指向本地 `third_party/yamux`。
- 存储: 默认 MySQL 8.0.16+，单实例可选 SQLite；驱动分别为 `github.com/go-sql-driver/mysql` 和 `modernc.org/sqlite`，均不依赖 CGO。
- 鉴权: 本地 admin session cookie + 官网 SSO JWT bearer token。
- 配置: 运行时身份、域名和公开端点全部来自环境变量；真实 `.env` 不提交。
- 部署: Docker multi-stage build，distroless runtime，Nginx/Caddy 负责公网 TLS 和路由。

## 3. 架构设计

Relay 入口在 `cmd/relay/main.go`，启动顺序是：

1. `internal/config` 加载 `.env`，严格校验运行时身份、域名、公开端点及其他环境变量。
2. `internal/store` 按 `DATABASE_TYPE` 连接 MySQL 专用库或 SQLite 文件，并初始化对应的当前 schema。
3. 可选根据 `ADMIN_USERNAME`/`ADMIN_PASSWORD` bootstrap 本地管理员。
4. 创建 `proxy.Manager` 管理在线 Agent/ Desktop tunnel session。
5. 挂载 Admin API、Desktop API、public component API、`/tunnel` 和公网 Host 转发。

主要流量链路：

- `/api/admin/*`: 管理 API。支持本地 `tunnel_hub_session` cookie，也支持官网 SSO JWT；JWT 必须满足 `role=admin` 且 `scope` 包含 `tunnel`。
- `/api/desktop/*`: Desktop 注册 API。只接受官网 SSO JWT，要求 `scope` 包含 `tunnel`。
- `/tunnel`: Agent 或 Desktop 主动连入的 WebSocket。普通 Agent 使用 `Authorization: Bearer <token>`；Desktop 不带 Authorization，并在首帧 `ns=d` 的 `tunnel.open` 中发送 `identityToken + deviceId`。
- 普通服务 Host: 通过 `routes.public_host` 找 active route，打开对应 token 的 yamux stream，转发 HTTP/WebSocket 到 Agent 本地服务。
- `*.m.example.test`: Desktop public Host。WebSocket upgrade 请求进入 Relay，向 Desktop tunnel stream 发送 `ns=d` / `desktop.websocket.open` 元数据；普通 HTTP 由宿主机反向代理转发到 `tunnel-hub-public`。
- `*.m.example.test/api/upload`: Mobile 上传入口，只从请求 Host 确定 Desktop，内部发送 `ns=ap`, `type=/api/upload`；multipart 不允许携带 `publicHost`。
- `*.m.example.test/api/resource`: Mobile 资源入口，内部发送 `ns=ap`, `type=/api/resource` 和 `{file,pushURL}`；Desktop 通过 ticket 保护的 `/api/push/{id}` 回推文件。
- `*-wa.example.test`: Desktop WebApp public HTTP/WebSocket。Relay 通过 WebApp route 与所属 deviceKey 打开 Desktop stream，向 Desktop 发送 `ns=wa` 的 `http.request` 或 `websocket.connect` 元数据。
- `share.example.test`: 分享只读站点。公开边缘网关将 `/share/*` 和 `/assets/conversation-export/*` 转发到 Relay；Tunnel API origin 也暴露对话模板。Relay 按冻结描述提供内置对话、隔离业务预览或明确支持的动作；对话 `ConversationSnapshotV1` 继续注入当前唯一模板。所有分享仅在所有者撤销后失效，公开读取不建立领取会话。对话模板和 manifest 由 WebClient 的独立发布命令替换，历史内容寻址资产保留，并从编译期 `embed.FS` 返回。

## 4. 目录结构

- `cmd/relay`: Relay 进程入口，负责 API 挂载、数据库初始化、静态站点兼容托管和公网转发分派。
- `cmd/agent`: 通用 Agent 进程入口，连接 Relay 并转发到本地 HTTP/WebSocket 服务。
- `internal/admin`: 管理 API、本地登录、SSO JWT 管理鉴权、overview/activity/metrics 聚合、公开 component 列表。
- `internal/auth`: secret hash、admin password、SSO JWT 验证。
- `internal/config`: Relay/Agent 环境变量配置和 `.env` 加载。
- `configs`: JWT 公钥等部署文件；真实密钥材料不提交。
- `tools/moduleprep`: 在临时工作树中把中性 Go module path 替换为构建环境路径，并执行 build/test/run。
- `tools/neutralcheck`: 使用外部禁用词扫描 Git 跟踪文件和待提交的新文件。
- `internal/desktop`: Desktop device 和 Desktop WebApp 注册 API。
- `internal/proxy`: Relay/Agent 转发实现、yamux session、active agent manager、traffic event 记录。
- `internal/sharecontract`: 通用分享 v1 DTO、包内路径、选择参数和形态校验；不解释市场或接收方业务上下文。
- `internal/shareassets`: 公开对话显示模板、manifest、当前 asset-set、历史内容寻址资源集与服务端渲染器；新分享页只使用当前 manifest 指向的资源集。
- `internal/store`: MySQL/SQLite schema、连接、少量方言适配、共享 DAO 和领域模型。
- `internal/tunnel`: 隧道协议结构、JSON frame、WebSocket frame、Host/path/upstream 工具。
- `deploy`: Nginx/Caddy 示例配置。
- `third_party/yamux`: 本地替换的 yamux 依赖，除非明确修复复用协议问题，不要随意改。

## 5. 数据结构

核心表由 `internal/store/schema.sql` 和 `internal/store/schema_sqlite.sql` 定义：

- `admin_users`: 本地管理用户。
- `admin_sessions`: 本地管理登录 session，cookie 名为 `tunnel_hub_session`。
- `tunnel_tokens`: 普通 Agent tunnel token，仅存 hash 和 prefix。
- `routes`: public Host 到 target/token 的映射。
- `desktop_devices`: 用户维度的 Desktop 设备与随机 public Host。
- `desktop_webapps`: Desktop 设备下的 WebApp，绑定独立随机 `*.wa` Host 和 route。
- `agent_sessions`: 普通 Agent tunnel 在线历史。
- `desktop_sessions`: Desktop tunnel 在线历史，以 deviceKey 关联设备。
- `events`: 管理操作和系统事件。
- `traffic_events`: Desktop/WebApp/普通 route 的访问统计。
- `conversation_shares`: 一个 shareId 独占的内容、kind、冻结标题、描述、所有者、可选旧会话关联及撤销状态；原 snapshot_json 字节只保存一份。旧非永久记录必须先撤销，再移除到期/一次性列和 claims 表；保留既有撤销时间及永久分享 ID、内容和资源。新内部模型不含退役字段；当前已发布 Desktop 的七键响应仅在 HTTP DTO 中保留 null/false 常量。不恢复 HTML 上传或旧 Platform 凭据路径。
- `conversation_share_access`: 每条分享最近一次成功公开访问时间；热更新与 Snapshot BLOB 分表，不保存访问日志或次数。

数据库只包含当前有效模型，不保留未使用的 API key 表或历史字段迁移；MySQL/SQLite 差异仅限当前 schema 所需的显式适配。

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
- `PUT /api/desktop/devices/{deviceId}/webapps/{name}`；同路径 `GET` 只读查询当前所有者已有 active 发布及 webAppId，不注册或更新
- `POST /api/desktop/shares`, `GET /api/desktop/shares`, `DELETE /api/desktop/shares/{shareId}`
- `POST/GET /api/desktop/v2/shares`, `GET /api/desktop/v2/shares/limits`, `DELETE /api/desktop/v2/shares/{shareId}`
- `GET /share/{shareId}`；preview/resolve/desktop/download 行为按通用契约选择，未接入行为明确拒绝
- `GET/HEAD /share/{shareId}/preview/{logicalPath}`：仅显式包描述下的本 ID 资源，Fetch Metadata、严格 CSP、opaque sandbox 与外层精确 frame-src 一并保留，禁止放开 same-origin/宿主 bridge 或新增裸文件旁路
- `GET/HEAD /share/{shareId}?action=service`：只引用已核实归属的现有 WebApp 注册 ID，复验撤销/active 后跳原公网 Host；不新建代理、转发 SSO 或把撤销短链等同停止原服务
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
- 运行时身份字段事实以 `internal/config/brand.go` 和 `.env.example` 为准。不要恢复配置文件注入，也不要写入真实生产值。
- 提交态 `go.mod` 和内部 import 必须保持 `example.invalid/tunnel-hub-server`；任何环境对应的 module path 只能通过 `GO_MODULE_PATH` 交给 `tools/moduleprep`。禁止在工作区原地批量改写 Go 文件。
- `FORBIDDEN_BRAND_TERMS` 只允许来自已忽略的 `.env` 或 CI 外部变量；提交前必须运行 fail-closed 的 `make verify-neutral`。
- `.env.example` 的 SSO issuer 默认留空，并预留 public key file；需要调试 SSO 或 Desktop 注册时，必须同时配置 issuer 和有效 `configs/jwt-public.pem`。
- Host 匹配必须统一经过 `internal/tunnel.NormalizeHost` 或等价逻辑，避免大小写、端口、尾点导致 route 绕过。
- `proxy.Manager` 以 `{kind,id}` 维护在线连接：普通 Agent 使用 tokenId，Desktop 使用 deviceKey；同一 key 的新连接会原子替换旧连接。
- HTTP 请求体当前在 Relay 侧完整缓冲，限制由 `MAX_REQUEST_BODY_BYTES` 控制。涉及大文件、流式上传或 backpressure 的改动要重点测试。
- Desktop public Host 不使用 `deviceId`，由随机 label 加 `domains.desktopPublicBase` 生成；WebApp Host 由随机 label 加 `domains.webAppPublicBase` 生成。
- Desktop/Platform 原票据由 Desktop 侧校验签名；移动公网 WS/上传/资源同时要求独立官网证明并检查目标归属。WS 使用 `bearer.<token>` subprotocol，禁止 URL Token；官网证明不得透传 Platform。Hub 仅在现有在线 Manager 中持有 Desktop 上传的票据，WebApp 通过受控 bootstrap 取得绑定连接的不透明 Cookie。
- 附件 API 的 Desktop 身份只来自 `<desktop>.m.example.test` Host；不得从 body、query 或其他客户端字段接受 `publicHost` 覆盖。
- 管理 token 手动创建当前禁用；Desktop 注册不创建 tunnel token、device secret 或轮换凭据。
- 普通退出不撤销设备。主动移除后的重新注册只能用官网验签后的固定会话创建时间恢复原记录，比较和恢复必须在现有注册事务中完成；保留设备主键、名称和公网地址，不以 JWT 签发时间代替登录时间。
- 对话显示资产必须由 Agent WebClient 同一次构建同步，路径中的 64 位 hash 是不可变集合身份；handler 只接受白名单路径并返回长期 immutable、CORS/CORP 与 `nosniff` 响应头。永久分享存在时不得清理历史集合。
- Go 改动提交前运行 `gofmt -w` 和相关 `go test`。
- React/Vite 改动同时运行 `tunnel-hub-public` 的 `npm test` 和 `npm run build`。Public 镜像构建不得注入运行时标题；容器启动时必须通过 `PUBLIC_SITE_TITLE` 生成纯文本 runtime 配置。

## 8. 开发流程

后端本地验证（MySQL 回归先按 README 配置独立服务；SQLite 回归使用临时文件）：

```bash
cd tunnel-hub-server
make test
make test-sqlite
make verify-neutral
make run-relay
```

Agent 联调：

```bash
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:8080/tunnel make run-agent
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

## 存储约定

- `DATABASE_TYPE` 缺省为 `mysql`；MySQL 连接只来自 `MYSQL_*`，SQLite 路径只来自 `RELAY_DB_PATH`。`MYSQL_PASSWORD` 不裁剪，不输出 DSN 或完整配置。
- 使用 `database/sql` 和具体 `store.DB`，数据库差异只允许出现在连接、schema 和必要 SQL/事务分支；不引入 ORM、通用方言框架或 Repository。
- 全新 MySQL 库由应用逐表建表；全新 SQLite 库在事务中建表并写入版本。两者采用现有显式迁移；Desktop 撤销字段升级必须保留设备、公网地址、分享及资源。
- 使用 InnoDB、utf8mb4、区分大小写的标识符、UTC DATETIME(6)；`max_allowed_packet` 至少 64 MiB。
- 设备/WebApp 注册和最后管理员保护必须通过数据库约束和事务保证并发语义；分享公开读取只查同一撤销状态，不恢复领取 Cookie 或访问到期分支。
- MySQL 测试使用 `internal/testutil/mysqltest` 创建独立随机库，不得读取生产账号或静默跳过；SQLite 测试只使用 `t.TempDir()` 下的文件。清理失败必须明确报告。
- 保留本地历史数据库和旧 Docker 卷，不把它们当作代码清理对象。

## 9. 已知约束与注意事项

- 根目录当前是三个 sibling 项目，不是一个统一 git 根目录；不要假设可以在上级目录使用 git 历史或提交。
- `.env`、`.env.test.local`、历史数据库文件、JWT key、真实 token、`configs/*.pem` 都不能提交。
- 生产部署依赖反向代理正确转发 WebSocket upgrade；修改部署文档时要同时检查 wildcard Host 路由。
- `*.m.example.test` 的 WebSocket upgrade 必须继续直达 Relay；普通 HTTP 在生产反向代理层应转到 `tunnel-hub-public`。如果普通 HTTP 到达 Relay，Relay 仍会返回 upgrade required。
- `*-wa.example.test` 是 browser-facing WebApp 代理，不等同于 tester 中的 Desktop business namespace `ns=wa`。
- `third_party/yamux` 是本地替换依赖，改动需要说明原因并跑完整隧道测试。
- 如果环境限制导致无法运行验证命令，最终说明里必须明确列出未运行项和原因。
