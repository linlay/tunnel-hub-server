# 本轮验证记录

2026-10-19，全部为本轮重写后执行的结果。

| 项目 | 实际检查 | 结果 |
| --- | --- | --- |
| Tunnel | `GO_MODULE_PATH=example.invalid/tunnel-hub-server make test-sqlite` | 全量通过，包含 yamux；使用临时 GOCACHE |
| Tunnel 最终入口 | `TEST_DATABASE_TYPE=sqlite go test ./internal/desktop` | 通过；包含额度协议、21 MiB 单次分享及调低新建额度后读取历史内容 |
| Platform | `go test ./internal/config ./internal/conversationexport ./internal/conversation ./internal/server` | 四包通过 |
| Go 构建 | 两仓各 `go build -buildvcs=false` 主程序 | 通过，二进制仅写入临时目录 |
| Desktop | `build:main:prepared`、`typecheck:renderer`、`architecture:check`、两个 contract check、`i18n:check` | 通过 |
| Desktop 现有测试 | conversation-html-worker、conversation-worker-bundle、conversation-html-export、conversation-share-controller、tunnel-conversation-share-client | 31 项通过 |
| WebClient | `test:unit --runInBand`：resources.requests、conversationExport、conversationSnapshotV1、conversationPreviewFixtures、useConversationActions | 5 套件、144 项通过 |
| WebClient 检查 | `typecheck`、`check:boundaries`、`check:desktop-contract`、`check:i18n` | 通过 |
| 渲染发布 | `release:conversation-export`、`check:conversation-export-sync` | 通过；当前资源集 c7aa11284fd73021b4238ecd00977e0e161dad4c3376dfd2c2603315b568dcac |

本机 smoke 使用临时 HTTP fixture 和正式构建的 Desktop Worker，未启动真实 Platform/Tunnel 服务：23,068,742 字节快照生成 Desktop 与浏览器 HTML 均为 23,139,800 字节；降低 Tunnel 分享预算后拒绝。浏览器检查使用正式 Blob 组装函数，不声称执行了浏览器 UI 下载。Tunnel 的实际上传/存储/历史读取由上述 SQLite HTTP 测试覆盖。

最初普通沙箱运行 Desktop 网络测试被 EPERM 阻止，允许本机回环后重跑通过。Tunnel 直接运行 go test 未指定数据库类型时提示缺少 TEST_MYSQL_HOST，随后按项目既有 SQLite 模式执行通过，未连接真实数据库。

独立 MySQL 测试实例（TEST_MYSQL_*）与中性扫描外部名单（FORBIDDEN_BRAND_TERMS）均未提供；用户已确认记录限制。本轮不将 SQLite 结果等同于 MySQL 集成通过，不宣称中性扫描通过。无生产访问、提交、推送或部署。

## 必要性报告修复后的复验

- Platform：四个相关包再次全部通过；HTTP/工具额度检查已移到真实出口，空 Markdown 与 CORS 回归断言通过。
- Tunnel：保持 `Open(ctx, cfg)`，通过既有 MySQL 配置传递派生容量；SQLite 全量（含 yamux）通过，Relay 构建通过。
- Desktop：main 构建与同一组 31 项相关测试通过，覆盖更直接的附件段组装。
- WebClient：类型检查及三个受影响套件共 58 项通过，包含无声明长度时 chunk 超额、正文长度不符、超限后中断，以及实际越过旧 20 MiB 的 Blob 组装。边界与当前渲染集同步检查通过。
- 已删除上一轮未发布的 815c1fa 中间资源集及其 dist 副本，当前 c7aa112 渲染集保持字节不变。本轮不需要生成新渲染集。

外部 MySQL 与中性扫描限制仍如上；本轮没有生产访问或部署。

Desktop 既有 25 MiB benchmark 通过：20 次，Worker p95 12.466 ms，主线程最大 timer gap 2.029 ms；不改变既有性能门槛。
