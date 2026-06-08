# AGENTS.md

本文件给后续在本仓库工作的编码代理和开发者使用。请先读根目录 `README.md`，再按本文件约定改动。

## 项目结构

- `zenmind-tunnel-server`: Go 1.26 模块，模块名 `github.com/linlay/zenmind-tunnel-server`。
  - `cmd/relay`: Relay 入口，负责公网 HTTP/WebSocket、`/tunnel`、`/api/admin` 和可选前端静态托管。
  - `cmd/agent`: Agent 入口，负责连接 Relay 并转发到本地服务。
  - `internal/admin`: 管理 API、cookie session、路由和 token 操作。
  - `internal/proxy`: Relay/Agent 转发逻辑和 active agent 管理。
  - `internal/store`: SQLite schema、迁移和数据访问。
  - `internal/tunnel`: 隧道协议、WebSocket net.Conn 适配、header/path 处理。
  - `third_party/yamux`: 本地替换的 yamux 依赖，除非明确需要，不要随意改动。
- `zenmind-tunnel-website`: React + Vite + TypeScript 管理前端。
  - `src/lib/api.ts`: 管理 API client，默认 `VITE_API_BASE_URL` 为空。
  - `src/App.tsx`: 当前主要 UI 和页面状态。
  - `src/styles.css`: 全局样式。
- `deploy`: Nginx/Caddy 示例。
- 根目录 `Dockerfile`: 同时构建前端和 Relay/Agent，最终镜像默认运行 Relay。

## 常用命令

后端：

```bash
cd zenmind-tunnel-server
go test ./...
go run ./cmd/relay
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:8080/tunnel go run ./cmd/agent
```

前端：

```bash
cd zenmind-tunnel-website
npm install
npm test
npm run build
npm run dev
```

容器：

```bash
docker compose up --build
```

## 开发约定

- 优先保持改动聚焦，不做无关重构。
- Go 代码提交前运行 `gofmt -w`，并尽量补充或更新对应 `*_test.go`。
- 前端改动尽量沿用现有 React/Vite/Vitest 结构，不引入新状态管理库或 UI 框架，除非需求明确。
- 文档中的命令、环境变量、API 路径要以代码为准，特别是 `internal/config/config.go`、`internal/admin/server.go` 和 `src/lib/api.ts`。
- 不要提交本地生成物：`*.db`、`*.db-*`、`node_modules/`、`dist/`、`*.tsbuildinfo`。
- 根目录当前没有 `.git` 元数据时，不要假设可以使用 git 命令查看历史或提交。

## 行为边界

- Relay 通过 Host 匹配 active route；新增转发能力时要确认 Host 归一化逻辑是否仍在 `internal/tunnel.NormalizeHost` 中统一处理。
- 当前 `proxy.Manager` 只维护一个 active agent，新 Agent 连接会关闭旧连接。支持多 Agent 或按 token/route 分流会影响核心设计，需要同步更新 README、测试和管理 UI。
- HTTP 请求体在 Relay 侧完整缓冲，限制由 `MAX_REQUEST_BODY_BYTES` 控制；大文件/流式上传相关改动需要特别测试。
- WebSocket 走自定义 frame 转发，帧大小限制在 `internal/tunnel/protocol.go` 中。
- 管理 session 使用 `COOKIE_SECRET` 签名。生产部署文档和配置示例必须提醒用户设置稳定强 secret。

## 推荐验证

按改动范围选择验证：

- 后端协议、路由、存储、鉴权、配置：`cd zenmind-tunnel-server && go test ./...`
- 前端 UI/API client：`cd zenmind-tunnel-website && npm test && npm run build`
- Docker/部署相关：`docker compose up --build`，并用 `Host: admin.localhost` 或实际域名访问 Relay。

如果因为环境限制无法运行某项验证，请在最终说明里明确写出未运行的命令和原因。
