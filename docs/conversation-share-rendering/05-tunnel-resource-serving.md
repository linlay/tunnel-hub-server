# T4：Tunnel 上传校验、资源响应与 HTML 外壳

状态：待执行。责任仓库：`tunnel-hub-server`。合同以 [01](01-solution.md) 为准。与 T5 提前共同完成最小真实浏览器验证，再展开完整预览。

## 1. 责任与入口

Tunnel 保持具体 `store.DB`、现 sharefiles 和 handler 结构；无数据库 schema 变更，无 Repository 或新增资源服务。它只验证与服务已上传资源，不解析历史、不发现 HTML 依赖、不访问 Platform 文件。

| 现有文件 | 修改目的 |
| --- | --- |
| [conversation_share_upload.go](../../internal/desktop/conversation_share_upload.go) | 放行规范当前 Chat sourceRef，校验新增关系及最终资源清单 |
| [conversation_share_attachment.go](../../internal/desktop/conversation_share_attachment.go) | 媒体 Range、HTML bootstrap、原文件下载 |
| `internal/desktop/conversation_shares.go`、`server.go` | 保持授权/路由/公开响应头，必要时接入现 Bundle |
| `internal/shareassets/handler.go` | 从当前 manifest 读取专用 bootstrap 资源及 integrity，生成可信外壳 |
| `internal/sharefiles/store.go`、`internal/store/conversation_shares.go` | 复用文件存储与授权读取；无需求不改 |

## 2. 上传校验

1. 沿用 snapshot 第一 part、后续 `attachment:<id>`、暂存、总量有界、hash/size 校验和提交/失败清理。Snapshot 中仅成功冻结项对应上传文件。
2. 将 artifact-only 的 sourceRef 语法检查改为规范 Chat 相对文件引用检查，保留旧路径合法性；严格拒绝绝对路径、空段、控制字符、重复编码遍历、非法 scheme 和反斜杠等。
3. 校验 01 的新增字段：ID 唯一、成功/省略不重叠、映射目标存在、只有 HTML 携带 `mediaRefs`、成功映射目标是媒体。节点引用可指向有 ID 的省略项，用于占位。
4. 只在现解析入口增加必要的薄结构和校验；不复制 Platform 的完整 Timeline 类型、HTML parser 或 MIME 检测体系。大小仍使用现 limits，错误合同拒绝整次创建。
5. 保留可选字段于原 Snapshot JSON；旧快照没有新字段时正常接受。未知或无效新关系不能默默丢弃后上传一个部分损坏的快照。

## 3. 资源响应

所有分支先执行同一个现有分享资源读取授权：shareId/resourceId 对应、到期/撤销、一次性浏览器会话均有效，随后才打开文件。HEAD、Range、`embed=1` 都不能旁路授权。

| 路由 | 新行为 |
| --- | --- |
| `GET/HEAD /share/{id}/attachments/{rid}/download` | 返回原文件，attachment disposition；复用 ServeContent 的 HEAD/Range 行为 |
| `GET/HEAD .../preview`，图片/音频/视频 | 正确 MIME、inline disposition，鉴权后 `http.ServeContent`；不重新生成或转换文件 |
| `GET/HEAD .../preview`，HTML，无 `embed=1` | 鉴权后重定向到 `/share/{id}?attachment={rid}`；HEAD 不认领一次性分享 |
| `GET/HEAD .../preview?embed=1`，HTML | 鉴权后返回可信网络 bootstrap 文档，HEAD 不返回正文 |
| 不支持的 preview MIME | 保持明确不可预览响应，前端使用 download 回退 |

使用标准库 [ServeContent](https://pkg.go.dev/net/http#ServeContent)，不手写 Range 协议。测试 200/206/416、HEAD、后缀 Range、无效范围及跨资源请求；不要预先设置冲突的固定 200/Content-Length。延续 no-store、nosniff、no-referrer 等公开分享响应头，授权发生在条件/范围响应之前。

SVG 仍仅作为图片资源，浏览器直接导航时也不能执行其脚本。下载原 HTML 继续有强制下载语义和限制性 HTTP CSP sandbox，不能因为支持交互预览就把下载变成可执行原文。

## 4. HTML bootstrap 与资产

1. 只扩展现 preview 路由，不新增公共认证体系。`embed=1` 是固定展示分支。
2. 返回固定小型外壳，不把原用户 HTML 注入该响应。HTTP CSP 与 01 第 8 节一致，包含 `sandbox allow-scripts`；绝不加 allow-same-origin。
3. T5 新增独立 `html-preview-bootstrap.js` 构建入口，随 renderer 集合发布。Tunnel 从现 manifest 的 `files` 中读取此固定文件的 path/integrity；复用已有 hash 目录和 SRI 数据，不新增第二个 manifest、动态资源注册表或专用 CDN。
4. 在现 Bundle 中增加最小专用渲染/资源信息方法即可；由服务端可信 origin 构建 URL，正确转义 origin 和 integrity，不接受用户传入脚本地址。
5. bootstrap 脚本使用匿名 CORS + SRI，继续依靠现 `/assets/conversation-export/{hash}/...` 的 CORS/CORP；附件本身不增加跨域可读权限。
6. 原 HTML 和已声明媒体由可信父页走 download 读取后一次交付；Tunnel 不重复打包/内嵌成 data URI，也不生成资源请求桥。

## 5. 必须删除的旧逻辑

新隔离链路可用后删除此 preview 路径上的 `sanitizeSharedHTML`、仅供其使用的标签/CSS正则及相关死代码，更新旧“所有脚本必须被删”测试。保留有价值的授权/下载/头部断言，不把旧清洗器作为隐藏降级路径。

如某 import/依赖还有真实其他调用方则保留；不为减少行数删除安全校验。当前一次性 Cookie、认领事务、过期/撤销逻辑和历史资源集不属于清理对象。

## 6. 验收与命令

- [ ] 旧 artifact 上传与新普通媒体上传均成功，part/描述符不一致、hash不符、非法路径/映射都失败并清理暂存。
- [ ] GET/HEAD/Range 均经过授权，陌生浏览器、撤销、过期和未知资源一致拒绝；附件请求不能认领一次性分享。
- [ ] 图片和音视频使用原始字节；跳播和后缀 Range 正常，不支持编码不触发服务端转码。
- [ ] HTML 无 embed 重定向，embed 返回可信外壳；非法参数不能更改身份或文件选择。
- [ ] CSP 隔离、独立网络 Document、一次性 Cookie、CORS/SRI 与 T5 在真实浏览器验收；不能仅凭响应字符串测试宣称安全链路通过。
- [ ] 当前及历史 assetSet 可访问，旧 hash 文件内容未改变；不创建/迁移数据库表。

执行前读本仓 README/AGENTS。Go 保持中性 module path，所有命令使用 `GOTOOLCHAIN=local`；按实际代码修改运行 gofmt、相关测试。SQLite 聚焦验证可沿用：

```sh
GOTOOLCHAIN=local go run ./tools/moduleprep exec -- env TEST_DATABASE_TYPE=sqlite go test ./internal/desktop ./internal/shareassets
```

提交前按仓库要求完成 `make test-all`、`make verify-neutral`。MySQL 使用独立测试账号/随机库，外部禁用词名单来自约定配置；缺少环境就记录阻塞，不临时改名单或使用生产库凑通过。

## 7. 交付与清理

交付 HTTP 合同、CSP、bootstrap 资产接入点、兼容性结果给 T5/T6。T6 统一同步最终资产，禁止手工编辑已压缩 runtime 或旧 hash 目录。

核对暂存文件、临时 SQLite、测试 MySQL 库和测试服务均已按归属清理；保留历史分享数据库、卷、资源及真实配置。执行最终 Git status/diff 和配置恢复检查。

执行记录（实施后填写）：实际变更；删除项；验证结果；未完成项；清理结果。
