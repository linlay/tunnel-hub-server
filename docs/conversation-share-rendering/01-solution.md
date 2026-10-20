# 最终技术方案：共享渲染边界与公开附件预览

日期：2026-10-20。状态：已确认的实施方案；尚未实施。任务导航见 [00 总览](00-overview.md)。本文是本次跨仓协议和技术边界的唯一依据。

## 1. 问题与最终行为

现有公开分享复用了时间线展示，但部分后代组件仍直接访问主应用能力。成功的图片生成结果会走到需要 `AppProvider` 的打开逻辑，引用和 Planning 也有类似边界问题。公式、图表动态加载和复制等异步操作另有未捕获失败；仅增加 React ErrorBoundary 不能解决这些问题。

现有资源链只从已发布 artifact 清单生成附件，生成图片、用户上传的当前 Chat 媒体可能未被收集。公开附件只预览清洗后的 HTML，脚本和多数媒体被移除，其他类型只支持下载。

最终行为：

- 主会话、应用内预览、公开分享、下载 HTML 使用同一展示结构，宿主只注入其实际拥有的交互能力。
- 公开分享支持 HTML、图片、视频、音频；其他已发布文件继续可下载。
- 分享冻结已发布附件及被分享内容明确引用的当前 Chat 媒体。缺失或超额资源逐项说明，正文与可用资源仍能分享。
- HTML 支持内联脚本及公开 HTTPS 静态依赖，运行在隔离文档内；本地直接媒体随分享冻结。它不是完整网站打包器。
- 渲染产物继续显式同步到 Tunnel 并随 Relay 发布；分享内容不做持续同步。

## 2. 职责与唯一处理位置

| 模块 | 负责 | 不负责 |
| --- | --- | --- |
| Platform 现有快照投影与导出入口 | 从选定历史投影节点；发现资源引用；规范化当前 Chat 路径；补充文件元数据；提供受限读取 | 分享预算选择、公开 URL、预览 UI、递归打包 |
| Desktop 现有 worker/controller | 读取候选文件、核验并冻结字节；按 Tunnel 额度取舍；生成最终清单；上传和反馈 | 再解析会话找资源、渲染 HTML、代理远程依赖 |
| Tunnel 现有分享 handler/sharefiles | 校验声明与上传一致；保存快照/文件；分享会话鉴权；按 HTTP 语义返回资源及可信 bootstrap 外壳 | 理解工具结果、访问 Chat/Workspace、发现 HTML 依赖、转换媒体 |
| 共享时间线组件 | 纯展示、局部失败态；消费现有 interaction context | 主动创建 AppContext、Desktop bridge、资源请求或导航回退 |
| WebClient 导出根与附件预览 | 从冻结清单解析资源；统一预览分派；隔离 HTML 组装；操作反馈 | 回连 Platform、找文件、修补缺失快照、通用子页面 RPC |

校验可以存在于各自信任边界，但内容发现只在 Platform 一处执行。Desktop/Tunnel 检查字节与协议，WebClient 按显式映射展示，不能各自发明另一套发现规则。

## 3. 资源范围、类型与额度

### 3.1 哪些资源进入候选目录

1. 保留现有当前 Chat 已发布 artifact 清单，所有合法文件类型均可作为下载附件。
2. 追加本次分享投影中明确引用的当前 Chat 图片、音频、视频：用户 query/steer 的文件引用、图片生成等结构化工具结果中的媒体引用、实际输出 Markdown 中的媒体引用。
3. 对已发布 HTML，收集其静态直接媒体引用。沿用 HTML 解析能力识别 `img[src/srcset]`、`audio[src]`、`video[src/poster]`、`source[src/srcset]`；同一引用只读一次。
4. 不扫描所有事件字段中的任意字符串、工具输入路径、整个 Chat 目录、Workspace 或其他 Chat。不得从正文中碰到一个路径就读取文件。

HTML 相对路径以该 HTML 当前已发布的 Chat 目录为基准；`@chat/` 明确以当前 Chat 根为基准。artifact 发布会把文件复制到 `artifacts/<runId>/<basename>`，原 Workspace 目录关系无法恢复，不作猜测。

首版支持上面的静态媒体属性。内联 CSS 和 JS 原样作为文档内容保留，但不分析 CSS `url()`、脚本动态拼接地址或 `fetch()` 来发现本地依赖；不打包本地 JS、CSS、字体、JSON，不递归解析其他 HTML/CSS。此范围在创建反馈和预览说明中明确，不能宣传为“任意 HTML 完全离线复现”。静态属性中不受支持或不可读取的本地引用显示原因。

### 3.2 类型白名单的实际含义

存储准入和预览支持分开：已发布附件没有仅限四类的业务白名单；未发布的当前 Chat 文件本次仅收集图片、音频、视频。元数据判定复用 Platform `documentmeta`，不只信调用方给出的 MIME 或文件后缀。

公开预览按规范 MIME 分类：`text/html`、`image/*`、`audio/*`、`video/*`，其余进入下载回退。图片和媒体的具体格式、编码是否可播由浏览器能力决定；不做转码、缩略图服务或 Office 转换。错误 MIME 不通过任意内容嗅探在父页面执行；SVG 通过图片元素展示，不以内联 SVG 注入父 DOM。

### 3.3 额度和失败语义

- Tunnel 的 `CONVERSATION_SHARE_MAX_SNAPSHOT_MIB`、`CONVERSATION_SHARE_MAX_RESOURCES_MIB` 是创建分享额度来源，默认各 50 MiB；资源额度是全部附件合计，不是每个文件 50 MiB。
- Desktop 每次创建继续查询 `/api/desktop/shares/limits`；不增加前端默认值，不往此严格响应结构顺手追加能力字段。
- Platform 导出额度继续由 `conversation-export.max-snapshot-mib` 及现响应头提供。沿用已有有界读取器。
- 延续现冻结总时限，不增加后台重试系统。时限到达后，所有尚未处理候选都记录 `timeout`，不能提前 `break` 后只留一个布尔值。
- 单资源缺失、拒绝、格式不在收集范围、过大、超时、变化或读取失败：部分成功，保存原因。分享正文无效、最终元数据仍超额度、上传或存储失败：整次创建失败，不能误报部分成功。
- 不建立跨文件事务快照。冻结时实际读取的字节是分享内容；已发布 artifact 仍核对发布时 size/hash。普通媒体有可用预期 size/hash 时核对，否则由 Desktop 为实际字节计算最终值。

## 4. Snapshot V1 的最小扩展

版本保持 `1`，不修改数据库 schema，不增加候选/冻结状态字段。不建立第二套媒体对象。以下是新增字段的合同示意，现有字段继续保留：

```ts
// 现有 SnapshotAttachmentV1 / Platform AttachmentV1
interface Attachment {
  id: string;                 // 规范 sourceRef 的 SHA-256 前 12 字节，24 位小写 hex
  name: string;
  mimeType: string;
  size?: number;              // 兼容旧输入；新候选须有 stat 大小，上传前核验实际值
  sha256?: string;            // 上传 Tunnel 前必须存在，64 位小写 hex
  sourceRef: string;          // 当前 Chat 的规范、编码后的相对引用
  mediaRefs?: Record<string, string>; // 仅 HTML：原始静态媒体引用 -> 目标附件 id
}

// 现有 SnapshotNodeV1
interface NodeExtensions {
  attachmentIds?: string[];   // query/steer 等节点的文件展示顺序
}

interface OmittedResource {
  id?: string;               // 能形成规范 sourceRef 时必须存在
  sourceRef?: string;         // 与 id 成对；用于旧正文/工具结果引用定位
  name: string;              // 安全显示名
  reason: "not_found" | "forbidden" | "unsupported" | "too_large"
        | "timeout" | "changed" | "invalid" | "read_failed";
}

interface SnapshotExtensions {
  omittedResources?: OmittedResource[];
}
```

字段语义与约束：

1. Platform 导出时 `attachments` 是合法资源候选；Desktop 上传时它只含成功冻结项。上传的每项与 `attachment:<id>` part 严格一一对应，失败项绝不能占用其中的描述符。
2. 沿用现有稳定 ID 算法；按规范 `sourceRef` 去重，同一路径不重复上传。保留既有 artifact 顺序，再按投影中的首次引用追加媒体；HTML 的直接媒体紧随对应 HTML 追加。无需内容寻址去重服务。
3. `attachmentIds` 只保存引用，不在节点复制完整文件描述。WebClient 在现有 `snapshotV1PreviewData` 中转换成 `TimelineAttachment`，只选取安全字段，不透传原事件 `meta`、本地绝对路径、任意 runtime payload。
4. `mediaRefs` 的 key 是解析器得到的原始属性 URL 值或 `srcset` 单个候选 URL，HTML 实体已解码；value 指向同快照成功资源或有 ID 的省略项。映射处理使用 `Map`/安全自有属性访问，不把不可信 key 合并到原型对象。
5. `omittedResources` 记录最终未冻结项；能规范化路径时保留 ID/相对引用，不能规范化时仅保留安全名称和原因。相同资源只记一次，不泄漏绝对路径、token、原始后端错误。
6. 成功集合和省略集合的 ID 不相交；节点引用和 HTML 映射目标必须存在于两者之一。HTML 附件自身省略后不保留它的 `mediaRefs`，仍保留其省略记录供节点定位。
7. `mediaRefs` 只允许 HTML 使用，目标成功项只允许媒体类型。缺失/非法新字段不能靠对象展开或 `as unknown as` 混入渲染；服务端创建入口拒绝无效合同，前端对历史缺失字段按空集合处理。
8. sourceRef 只允许当前 Chat 的规范相对文件路径；沿用既有路径编码、遍历、内置目录及符号链接检查。Tunnel 校验语法，绝不拿它访问本机文件。普通文件与 published artifact 的业务授权仍由 Platform 判断。
9. HTML 原文不内嵌进快照、不重写后再存一份。保存原附件，预览时依据映射处理；下载始终返回原文件。

候选与最终附件使用同一现有结构即可：对新增普通媒体，Platform 不为生成候选而额外完整读一遍文件做 hash；已有 artifact 的 hash 保留，普通媒体的最终 hash 由 Desktop 冻结时产生。Platform 的 `SHA256` 增加 `omitempty`，缺省 hash 必须不出现在 JSON 中，不能输出会被 Desktop 判为无效的空字符串。新合法候选的 size 使用文件 stat 值；stat 失败记省略原因，不能用零表示未知大小。Tunnel 对最终上传仍强制 size/hash。

新字段一起计入快照额度。Desktop 保留正文只序列化一次的现有原则，仅调整附件和省略清单的小型元数据；必要时移除末尾成功项并新增原因，仍放不下则明确失败。不得为了让请求通过而静默截断原因或引用。

## 5. Platform 到 Desktop 的读取

现 `/api/resource` 支持更广的资源语义且有不同的主体归属判断；`/api/chat/artifacts/read` 的语义是已发布 artifact。不要放宽二者来凑本功能。

在现导出路由旁增加一个薄入口：`POST /api/chat/export/resources/read`，body 只有 `{ chatId, sourceRef }`。复用当前 Desktop app/device 身份认证和 Chat 存在性检查：

- 必须经过现认证中间件，且符合 Desktop runtime 的 app/device 身份；不能只检查请求里自报的设备字段。
- published 引用委托 `chatresource.OpenArtifactByRef`，保留 manifest、大小和 hash 校验。
- 其他引用经 `BuildResourceKey/ParseResourceKey`、`ResolveResource`、`documentmeta` 限制为当前 Chat 普通媒体文件；拒绝目录、内部路径、越界符号链接、绝对路径、其他 Chat 和任意 HTTP URL。
- 返回现有文件响应模式及可区分的错误码，供 Desktop 记录原因；不创建导出会话、临时授权票据或持久化候选清单。

资源请求错误约定：400 对应 `invalid`，401/403 对应 `forbidden`，404 对应 `not_found`，409 对应 `changed`，415 对应 `unsupported`；其余读取错误对应 `read_failed`。Desktop 的资源预算/有界读取超限对应 `too_large`，冻结截止或资源请求超时对应 `timeout`。用户取消整个创建不伪装为附件超时。现有读取器会合并部分错误，T2 必须在资源读取路径保留这些差异；快照/模板原有整体失败语义保持。

此入口的授权范围是当前 Chat 合法媒体/已发布文件；本次分享实际选择哪些文件，由 Platform 单次资源发现清单决定。不要为了再次证明“被引用”而在每个文件请求中重放全部历史。

## 6. 共享渲染和错误处理

继续使用 `ConnectedConversationStage/ConversationPreview → ConversationStage → TimelineRow`。把主应用认证 URL、下载、菜单和打开行为留在原有宿主，注入已有 `renderAttachment/openTarget/openSource` 等能力。图片生成卡片保留共同布局和状态，通过现有附件渲染能力承接图片；不为文件格式增加 `renderPDF/renderCSV/...`。

生成图片的业务 artifact/reference 身份与快照资源 ID 不同。宿主适配必须保留原打开类型、加载后打开和失败重载行为；不能直接把快照 ID 填入主应用的 `referenceId`，也不能假定普通 AttachmentCard 是旧生成图片 Tile 的等价替代。

必须消除的耦合包括 `ImageGenerationCard` 内主应用 hooks/请求回退、`ReferenceCard` 的无条件 `useOpenTarget()`、Planning 的不匹配消息上下文，以及无能力仍显示的打开按钮。主会话依然由宿主注入完整能力，只读入口只提供真实可用动作。

公开导出根从成功/省略目录解析 Markdown、生成图片及节点附件；不回退到原 `/api/resource`、本地路径或主应用认证接口。未知本地资源显示不可用；普通外部链接按现有安全规则显示。

错误策略：

- 公式/图表/媒体等预期失败在该内容块显示源码、原因或下载；捕获动态 import、初始化、解析和播放失败，失败的 Promise 缓存不得永久污染后续尝试。
- 复制、打开、下载操作在操作处反馈。下载链接交由浏览器处理时不虚报已完成；实际捕获到的错误才反馈。
- 未预期 React 渲染异常由一个导出页面级 ErrorBoundary 兜底；同步启动/解析失败仍由现有启动错误页处理。无需每卡一个边界或通用恢复调度器。
- 不以挂载完整 `AppProvider` 修复公开页面；不为同一展示卡复制分享专用版本。

现导出边界脚本应检查导出入口的实际构建依赖图，包括异步 chunk，忽略 type-only import；禁止主应用 runtime 进入包。升级现脚本即可，不增加独立架构检查平台。

## 7. 普通附件预览和扩展方式

在现 `ShareAttachmentViewer` 保留文件切换、工具栏、下载、复制链接和通用错误展示，只在一个入口分类、分派 HTML/图片/视频/音频/不支持类型。链接继续通过 `shareAttachmentLinks.ts` 生成，独立页面沿用 `/share/{id}?attachment={resourceId}`。

图片使用 `<img>`，音视频使用原生 `<audio controls>` / `<video controls>`，不自动播放。媒体直接使用当前分享同源预览地址；Tunnel 在鉴权之后调用 `http.ServeContent`，提供 GET、HEAD、Range、跳播和正确 MIME。媒体加载失败提供可见原因或通用失败提示及原文件下载。

对于已冻结且可由浏览器/前端直接读取的普通附件，未来预览接入步骤只有：扩展此处类型判断、实现只接收附件元数据/只读资源地址的视图、补对应验收。已发布并冻结的文本/Markdown/CSV 通常不改后端；PDF 复用可用解析能力并验证 worker/CSP；Office/CAD 需要真实解析/转换能力，届时再单独设计，不能提前预埋转换服务。若要把尚不在自动收集范围的文件纳入分享，这是资源准入需求，另行评估，不能与增加展示格式混为一谈。

分类在前端只有一个公共函数/分派点；Tunnel 的 MIME 响应限制是独立 HTTP 安全边界，不是另一套 UI 类型系统。不建插件注册中心、不让每个格式复制工具栏，也不扩展 TimelineInteractionContext 为格式方法集合。

## 8. HTML 预览的隔离流程

### 8.1 必须采用网络文档 bootstrap 的原因

一次性分享 Cookie 当前为 `HttpOnly; SameSite=Lax; Path=/share/{id}`。opaque-origin sandbox iframe 的内部请求不能依赖该 Cookie；前期本机合成探针已观察到父页面和 iframe 导航带 Cookie，但 guest 内脚本/图片/fetch 请求不带。

同时，`srcdoc`、由父文档创建的 blob/data 文档存在 CSP 继承限制，不能用子文档 meta 放宽父页面脚本策略。因此本方案采用现有 preview 路由返回的独立网络文档，不使用父页面 `srcdoc` 或 blob/data 文档作为执行入口。[CSP 继承规范](https://www.w3.org/TR/CSP3/#security-inherit-csp)、[sandbox 说明](https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Elements/iframe)。

### 8.2 最小完整流程

1. 用户在现 `ShareAttachmentViewer` 选择 HTML。可信父页面凭当前分享会话，通过已有 `/download` 读取选中的 HTML 和它 `mediaRefs` 指向的成功媒体；不把所有附件都传入子页面。
2. iframe 请求现 `/share/{id}/attachments/{resourceId}/preview?embed=1`。此参数只选择响应形式，绝不是鉴权开关。Tunnel 先完成分享、资源、过期、撤销和一次性会话校验，再返回小型可信 bootstrap HTML。
3. iframe 属性固定 `sandbox="allow-scripts"`，不加 `allow-same-origin`。响应的 HTTP CSP 也包含 `sandbox allow-scripts`。bootstrap JS 从本次不可变 renderer asset set 加载，使用既有匿名 CORS、SRI 和 CORP；它不需要分享 Cookie。
4. bootstrap 在用户脚本运行前建立一个 MessageChannel，将端口交给父页。父页只接受当前选中 iframe 的 `event.source` 和当前装载状态；`origin === "null"` 单独不能证明身份。子页校验父窗口和明确的分享 origin。每个 iframe 实例只接受一次初始化，旧实例/重复消息被忽略。
5. 父页通过该端口一次性交付 HTML、固定映射及目标媒体 Blob/可转移字节。端口绑定最初的 bootstrap 文档，防止仅持有可导航 WindowProxy 后误投递；交付后双方关闭端口及消息监听，再执行用户 HTML。
6. guest 自己创建 Blob URL，在自身文档里解析 HTML 并机械应用映射，随后在该网络 bootstrap 的 Document 内装载和执行内容。可复用 DOMParser、纯 HTML 序列化工具；使用 `document.open/write/close` 时必须验证 HTTP CSP 和 sandbox 持续生效。不得再次导航到父页创建的 blob/data/srcdoc。
7. 同一资源多次引用只创建一个 Blob URL；缺失项在对应媒体处显示原因，并在可信工具栏保留可见摘要。切换/关闭时中止父页请求、关闭端口、销毁旧 iframe；释放资源，禁止迟到结果覆盖新选择。

这个通道只有一次固定数据交付，不接受任意 URL、后续资源请求、宿主操作或 RPC。HTML 内引用的本地媒体在开始执行前已加载，故其大视频不是网络 Range 流式播放；独立视频预览仍使用 Range。

### 8.3 HTTP 与 CSP 约束

- HTML `/preview` 不带 `embed=1` 时，在鉴权后重定向到现有 `?attachment=` 页面；一次性分享直接附件请求不负责认领，须先打开分享主页。
- 父分享页面保持原严格脚本策略，仅增加普通同源图片/媒体显示所需的 `img-src/media-src`；不允许父页执行附件内联脚本。
- guest HTTP CSP 从 `default-src 'none'` 开始，允许内联脚本/样式和公开 HTTPS 的 scripts/styles/fonts/media；映射后的图片/媒体允许 blob，既有合法内嵌媒体可允许 data。使用 `connect-src 'none'`、`object-src 'none'`、`frame-src 'none'`、`worker-src 'none'`、`form-action 'none'`、`base-uri 'none'`、`frame-ancestors 'self'`；不开放 eval、顶层导航、弹窗和表单能力。
- 外部 HTTPS 资源由浏览器直接加载；需要 CORS 的模块/字体仍服从资源方策略。没有 CDN 代理、服务器抓取、资源缓存或可用性保证。
- `connect-src 'none'` 禁止 fetch/XHR/WebSocket 等连接型 API；允许 HTTPS 静态资源仍会产生外网请求，不能把此设计描述为完全无网络或阻止一切外传。
- 原 HTML 下载保留 attachment disposition、`nosniff` 和限制性 HTTP CSP sandbox，不通过原文下载响应提供可执行页面。
- 不修改 Cookie SameSite，不增加 URL token，不增加域名，不把附件资源开放跨域读取，不借 guest 传递身份凭据。

### 8.4 证据和未完成验证

已完成的合成探针仅证明当前浏览器中 opaque iframe 不携带 Lax Cookie；临时服务、Cookie 和页签均已清理。完整 bootstrap 尚未实现。Chrome、Safari、Firefox 的一次性分享、内联/外部脚本、Blob 媒体、CSP 持续生效、消息竞态和导航行为是 T4/T5 的最早验收门槛，不能把规范分析当成浏览器测试通过。

## 9. 兼容、同步和发布

1. 新字段可选，新 renderer 必须接受旧 Snapshot V1；新 Tunnel 继续接受旧 artifact 路径和旧 multipart。没有数据库迁移或历史快照批处理。
2. 旧 Tunnel 会拒绝非 `artifacts/...` 的 sourceRef，旧 Desktop 会丢弃新增映射；可选字段不等于任意混版兼容。先部署新 Tunnel 与匹配 renderer，再发布配套 Platform/Desktop，按现内置服务发布链交付，不增加能力协商接口。
3. WebClient 通过现 `release:conversation-export` 显式构建和同步；包含新增 bootstrap 资源及动态 chunk，当前 manifest/template 与该集合一致。普通主 WebClient 构建不能顺便发布 Tunnel 资产。
4. 在线分享从已保存快照和当前模板生成，更新 renderer 后旧分享可获得展示修复。未曾冻结的媒体仍缺失，需要重新创建分享。
5. 已下载 HTML 引用其构建时的 hash/SRI；不能远程修复该文件。保留所有历史 assetSet，不覆盖同 hash 内容，不清理仍可能被引用的旧资源。
6. 新快照创建后回滚到旧 Tunnel 可能破坏资源合同；部署前记录匹配版本，优先回滚到能读取新合同的兼容版本。任何回滚都不得删除分享、附件、数据库或历史资产。

## 10. 架构验收标准

方案是否“不过度设计”以实际变更判断：资源只发现一次；宿主能力只在宿主；预览类型只有一个 UI 分派入口；没有新持久化模型；没有通用插件/RPC/同步层；没有重复展示卡片；替代代码已经删除。HTML 的专用 bootstrap 是脚本隔离和既有一次性 Cookie 共同要求的最小额外机制，其边界不得扩大。

具体代码入口、测试和删除清单见 T1–T6；整体验收、发布及清理以 [07](07-validation-release-cleanup.md) 为准。
