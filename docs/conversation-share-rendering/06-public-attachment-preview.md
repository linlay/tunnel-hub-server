# T5：公开附件预览与隔离 HTML

状态：待执行。责任仓库：`agent-webclient`。依赖 T3 的共享渲染边界、T4 的资源响应；真实数据验收依赖 T1/T2。与 T4 先做 HTML 最小浏览器验证。合同以 [01](01-solution.md) 为准。

## 1. 责任与入口

本任务消费已冻结资源和明确映射，不能重新扫描会话历史，也不能回连 Platform 或查询 Workspace。普通格式扩展的唯一 UI 入口是现有附件预览组件。

| 现有入口 | 修改目的 |
| --- | --- |
| [ShareAttachmentViewer.tsx](../../../agent-webclient/src/export/ShareAttachmentViewer.tsx) 及 CSS | 共用工具栏、类型分派和错误/下载回退 |
| [ConversationExportDocument.tsx](../../../agent-webclient/src/export/ConversationExportDocument.tsx) | 会话内引用、独立附件页、选择状态、缺失占位 |
| `src/export/shareAttachmentLinks.ts` | 保持现 URL 生成，扩展 HTML embed 地址 |
| `src/export/conversationSnapshotV1.ts` | 与 T3 对接新字段，禁止重复定义另一份快照模型 |
| `src/shared/i18n/conversationExport.ts` | 中英文原因、加载与失败文案 |
| `src/shared/utils/htmlVisualizationAssets.ts` | 复用纯 `serializeHtmlDocument` |
| `webpack.export.config.js`、`scripts/build-conversation-export-template.js`、现同步/检查脚本 | 增加小型 bootstrap 构建入口及 CSP/资产校验 |

可为 HTML 附件增加一个专用组件和不依赖 React/App 的 bootstrap 模块；这是两种运行环境的实际分工。不要建立通用 preview engine、插件注册器或宿主消息 SDK。

## 2. 普通预览与引用展示

1. 在一个函数/分派点按规范 MIME 判断 HTML、图片、视频、音频和下载回退。浮层、独立页面、会话链接都复用它，不各写一份 `mimeType === ...` 规则。
2. 保留文件切换、键盘操作、焦点、关闭、下载和复制链接行为。普通图片/媒体视图只接收元数据与现 resource URL，使用原生元素和加载/错误态。
3. 成功与省略目录分别建索引，通过 ID/sourceRef 解析节点附件、Markdown 及生成图片。优先使用明确关系；旧格式引用仅做规范化匹配，不模糊用任意 URL 尾缀命中另一个文件。
4. 公共 Markdown 不再把所有图片统一替换成文字。能命中冻结媒体则展示，省略项显示名称/原因；未知本地引用明确不可用，不回退到原 Platform URL/凭据。
5. `/share/{id}?attachment={rid}` 继续使用现独立页；未知/缺失 ID 提供明确状态，不抛出全页异常。HTML/媒体预览失败时保留文件信息和下载入口。
6. 一次性分享继续禁止误导性的重新分享操作；直接附件访问先受 T4 授权规则约束。下载 HTML 没有 share 路径时只读降级，不伪造下载地址或打包媒体。
7. 错误文案区分创建时省略与打开时读取失败：前者来自快照原因码，后者可能是会话失效、文件损坏或编码不支持。原生媒体无法可靠细分时用准确的通用失败提示，不能伪造服务端原因。

## 3. HTML 的父页与 guest

严格落实 01 第 8 节，不另起协议。实现分两部分：可信父页负责受鉴权读取与生命周期，隔离 guest 负责机械替换和文档执行。

### 父页

- 请求当前 HTML 的原文件和 `mediaRefs` 中成功媒体；使用现 `/download`、同源 Cookie、有界读取和 AbortController。不接受 guest 提供 URL，不读取所有分享附件。
- 按声明的冻结 size 核对读取上限/完整性；沿用已有规范化元数据。取消/切换时中止读取，不新建长期缓存或下载重试队列。
- 创建网络 iframe，属性 `sandbox="allow-scripts"`、`referrerPolicy="no-referrer"`；src 为现 HTML preview embed 分支。
- 只接收当前 iframe/current load 的 bootstrap 就绪消息及其转移端口；校验来源窗口和消息形状，一次使用后注销。不得只信 `origin: null`，不得对任意 window 广播附件。
- 使用端口一次发送固定 payload，媒体可用 Transferable 减少拷贝；父页保留必要错误 UI，不留后续子页资源代理。

### Guest

- 独立小入口生成固定 `html-preview-bootstrap.js`，无 React/AppContext/认证/平台客户端依赖；由 T4 的可信外壳以匿名 CORS 和 SRI 加载。
- 校验父窗口与固定分享 origin；端口只消费一次。执行用户脚本前关闭端口和全局监听，后续消息一律不提供资源或宿主能力。
- 在 guest 内用 DOMParser 处理 HTML，依据 `mediaRefs` 定位 01 指定的静态媒体属性，替换为 guest 自己创建的 Blob URL；映射不到的本地媒体展示原因，不尝试真实路径。
- `srcset` 逐候选替换并保留描述符；同 ID 共用 Blob。不要用全局字符串替换损坏脚本、文本或 CSS。
- 使用纯序列化工具，在原网络 Document 内装载最终 HTML；验证 `document.open/write/close` 后 HTTP CSP 与 sandbox 持续生效。不能使用父页面 srcdoc/blob/data 导航绕过 CSP。
- 内联 JS/CSS 和公开 HTTPS 静态资源按既定政策运行；不分析脚本动态地址，不支持业务 fetch/WebSocket。HTTPS 允许规则只约束协议，不代表系统替资源方证明公网可达或持续可用。
- 缺失媒体在该位置展示原因，同时父页保留可信摘要。自定义 HTML 可以改变其自身内容，所以关键反馈不能只放在用户脚本可改写的 guest DOM。

复用 `DOMParser` 和 `serializeHtmlDocument` 即可。现 Markdown 的 `htmlPreviewDocument.ts` 有不同 CSP、固定依赖和高度桥；`viewDocument.ts` 有不同外链/内嵌策略，不能直接套用，也不把三者硬抽成全功能框架。

## 4. 构建与删除清单

- bootstrap 与主 renderer 同一次构建，进入同一 hash 集合及现 manifest；读取已有 `files[].integrity`，不再维护独立版本表。T4 需要的文件名固定在此合同中。
- 父页面 CSP 仅增同源图片/媒体，内联用户脚本只在 guest 放行。保留现样式 nonce、SRI、字体/公式等已存在的资产规则。
- 更新现产物检查，覆盖 bootstrap 缺失、SRI不符、意外动态依赖和主应用耦合；不手工修改 Tunnel 已发布文件。
- 删除原 HTML-only iframe 分支、无条件图片文本替代、重复 MIME 判断及模糊资源 URL 兜底。没有能力的本地 HTML 路径仍可见降级，不能误删必要分支。
- 不保留旧清洗预览作为“悄悄少功能”的回退；bootstrap 失败应可见，原文件可下载。

## 5. 验收与测试

扩展 `ShareAttachmentViewer.test.tsx`、导出文档与快照测试；为新的父页生命周期、静态属性改写、端口一次交付补聚焦测试。普通单元测试不代替真实浏览器门槛。

- [ ] 图片、音频、视频在浮层和独立页共用视图；视频跳播、播放失败/格式不支持有下载入口。
- [ ] 同一份快照在列表、正文、生成图片卡、用户文件节点和 HTML 内得到一致资源/省略结果。
- [ ] HTML 的内联交互、外部 HTTPS 脚本/样式/字体、冻结媒体正常；断网外部依赖失败不会让父分享页面崩溃。
- [ ] 只有合法当前 bootstrap 获得所需 payload；伪造、重复、切换、关闭、导航与迟到异步响应不泄漏或串用资源。
- [ ] guest 无法读取父 DOM/Cookie/存储，不存在 fetch/RPC 桥；HTTP CSP 在写入最终 HTML 后继续阻止连接型 API、表单、嵌套 frame、worker 和 object。
- [ ] 一次性分享同会话可看 HTML 和媒体，其他浏览器被拒绝；加载后撤销只阻止后续请求，不声称能追回已加载字节。
- [ ] 原生媒体只借 URL播放；HTML 内媒体一次加载进 guest，关闭/切换后没有遗留 iframe、端口和任务级缓存。
- [ ] 旧快照没有新字段仍可读，下载 HTML 不获得新增媒体打包能力。

运行 T3 所列前端检查与相关测试；最小 Chrome/Safari/Firefox 实验在实施早期完成，完整真实 HTTPS/一次性 Cookie 回归由 T6 汇总。没有可用浏览器就明确记录待验收，不把 jsdom 结果当作通过。

## 6. 交付与清理

向 T6 交付实际支持格式样例、HTML本地依赖范围、失败样例、浏览器版本/证据及构建输出。同步由 T6 统一执行，避免半套主 renderer/bootstrap 被发布。

清理自建测试分享、合成 Cookie、页签、服务、临时媒体和不需交付的构建文件；删除对象仅限本任务创建且归属明确的状态。不要访问真实一次性分享来测试，也不删除旧 assetSet。

执行记录（实施后填写）：实际变更；删除项；验证结果；未完成项；清理结果。
