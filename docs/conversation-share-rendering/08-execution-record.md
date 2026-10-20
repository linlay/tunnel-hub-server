# 对话分享渲染改造：执行与交接记录

日期：2026-10-20。

**本轮交付：T1–T5 代码、匹配渲染资产、已执行检查及浏览器证据。T6 发布验收尚未全部通过，不标记为可发布。** 用户已确认：独立 MySQL 测试配置和中性扫描名单缺失时，记录为待验证项，交付代码和检查记录。

第 1–6 节保留首次交付记录；后续边界修复和最终资产以第 7 节为准。

00–07 保留原始方案内容，其中“待执行”是编写计划时的状态；实际执行结果以本文为准。没有提交、推送或部署。

## 1. 代码与资产基线

四仓仍在原 `main`，以下为修改前及交付时未改变的 HEAD；实现位于各仓工作区，尚无新的提交 revision。

| 仓库 | HEAD |
| --- | --- |
| agent-platform | `55d70b9d5a4da4f042331a8a5e403aea117355bd` |
| zenmind-desktop | `73e3b3c8a64908baa70f50623cad4ba925eb906e` |
| agent-webclient | `cec817da264bc02514a09f38735d97a1650c0610` |
| tunnel-hub-server | `ae254c020545b38205058f9a5efe5272486ae494` |

最终 renderer assetSet：`a6216018f30dd8e3546db88dd95f563914979052e393cc93f02846f2f6fb8f59`。WebClient 同一源码构建的模板、manifest、bootstrap、runtime、样式、字体及其他资源已显式同步至 Tunnel；重复构建得到相同 assetSet，同步检查通过。新资源集包含 161 个文件，`runtime.js` 为 1,241,233 字节。

## 2. 实现与职责

| 任务 | 实际实现 |
| --- | --- |
| T1 Platform | 纯内容投影与文件发现分开；HTTP/工具使用同一文件感知入口，只发现一次。收集已发布文件及当前 Chat 明确引用的媒体，覆盖 query、人类 steer、成功图片生成和 Markdown；HTML 只提取直接媒体属性。新增受 desktop app/device/subject 鉴权约束的资源读取入口。 |
| T2 Desktop | Worker 顺序读取候选并冻结，校验候选已有 size/hash，兼容缺少这两项的旧候选；最终计算实际 size/hash。正文只序列化一次，按预算裁剪附件与元数据，统一输出逐项 omittedResources；创建弹窗和二维码弹窗消费同一清单。 |
| T3 共享展示 | 主应用通过现有 TimelineInteraction 注入复制、导航、附件及图片生成能力；共享组件不再隐式回退到主应用运行时。保留原图片生成 tile 行为。公式、Mermaid、ECharts 的失败局部降级并清理异步资源；Mermaid 最终 SVG 样式带父页 nonce。 |
| T4 Tunnel | 校验新增可选字段、规范 sourceRef、ID/引用关系及最终 size/hash；沿用现有存储和鉴权。媒体 GET/HEAD/Range 先授权；HTML 原文下载不变，普通 preview 跳转，embed 返回独立 CSP 文档和固定 SRI bootstrap。 |
| T5 公开预览 | 一个 MIME 分类入口分派原生图片/音频/视频和 HTML。HTML 使用无 same-origin 权限的网络 iframe、guest 创建的文档绑定 MessageChannel、一次固定 HTML/媒体交付和 guest Blob URL；缺失或不支持资源显示原因与下载回退。 |

已删除被替代的 HTML 清洗器、资源尾部路径匹配、共享组件的主运行时回退及分享遗漏布尔状态。没有新数据库表/版本、Repository、ViewModel、插件注册中心、资源代理 RPC 或持续同步任务。资源发现只属于 Platform，原始 HTML 只存一份，遗漏清单保持单一事实来源。生成资源没有手工修补。

## 3. 自动化检查

Go 使用 `GOTOOLCHAIN=local`；Tunnel 通过既有 `tools/moduleprep` 执行，未改中性 module path。构建二进制放在本次临时目录，未覆盖用户已有二进制。

| 仓库/检查 | 实际结果 |
| --- | --- |
| Platform：`go test ./internal/conversationexport ./internal/server ./internal/conversation ./internal/chatresource ./internal/chat` | 通过。包括发现范围、Markdown/HTML 引用、读取授权及投影回放；人类 steer 的新增输出已更新对应 golden。 |
| Platform：`go build -o <临时目录>/agent-platform ./cmd/agent-platform` | 通过。 |
| Platform：`go test ./...` | 未全绿。导出 golden 已修复；`TestNPMDiscoveryRefreshAndIsolation` 单独重跑通过。`TestDesktopControlReviewsMatchDesktopExemptions` 仍失败，其测试和对应 Desktop action-contracts 均与原 HEAD 相同，现有正则不匹配当前声明形式。 |
| Desktop：`npm test` 的架构、契约、i18n、main/renderer 构建阶段 | 通过；另行复核架构、双契约、main types、renderer typecheck 通过。 |
| Desktop：完整测试阶段 | 3,638 项，3,515 通过、82 失败、41 跳过。失败涉及现有源代码字符串断言、sidebar/skin/runtime 等模块；截图选择测试子进程长时间不退出，确认归属后终止并计入失败。未对全部失败逐项做原 HEAD 复现，不将它们一概认定为基线问题。 |
| Desktop：分享相关 5 个测试文件 | 35/35 通过：HTML 导出/Worker、分享 controller、弹窗 hook、Tunnel client。涵盖部分冻结失败、同一 Snapshot Buffer 上传、旧候选兼容、失败反馈。 |
| Web：`npm run build`、i18n、边界、Desktop 契约 | 通过；42 个 feature、0 个依赖环。 |
| Web：分享及相关组件定向测试 | 20 套件/112 项通过；另补 Mermaid nonce、图表清理、公式失败与选中文本能力注入检查，均通过。各轮存在重叠，不累加成独立测试数量。 |
| Web：最终 `npm test -- --runInBand --silent` | 461 套件：457 通过、4 失败；3,948 项：3,892 通过、56 失败。新增 Mermaid mock 问题已修复，最终失败仅为下述已复现基线套件。 |
| Web：`npm run release:conversation-export`、`check:conversation-export-sync`、`check:conversation-export` | 通过，包括实际 emitted module graph、异步 chunk、bootstrap 边界、hash/SRI 与同源资源校验。 |
| Tunnel：`make test-sqlite` | 全量通过，包括第三方 yamux；最后一次运行时浏览器临时测试入口已删除。 |
| Tunnel：经 moduleprep 构建 `./cmd/relay`、`./cmd/agent` | 通过。 |
| Tunnel：`make test-all` | 未通过：MySQL 测试缺少 `TEST_MYSQL_HOST` 等独立配置；SQLite 已单独执行通过。 |
| Tunnel：`make verify-neutral` | 未通过：未配置 `FORBIDDEN_BRAND_TERMS`；没有自行编造名单。 |

Web 的 4 个失败套件已从原 HEAD 创建隔离副本复现，结果同为 56 项失败、9 项通过：`LeftSidebar.test.ts`（AppProvider mock）、`ProjectConsole.test.tsx`（组件 mock）、`agentWebclientBridge.test.ts`（静态 hash 预期）、`tailwindPrefix.test.ts`（既有 SteerBar class 写法）。隔离副本及测试缓存随后按清理流程移除。

## 4. 实际浏览器检查

使用合成快照、临时 SQLite、SVG/WAV/VP8 WebM 及损坏 WebM，通过真实 Tunnel multipart 创建和 HTTPS 页面运行。Chrome 使用独立 profile；本地测试证书仅由该进程忽略校验，没有修改系统证书或用户浏览器配置。未使用用户真实分享链接、会话或 Cookie。

| 浏览器 | 结果 |
| --- | --- |
| Chrome `155.0.8059.80` | 已执行下列检查并通过。 |
| Safari `26.2` | 未完成：WebDriver 创建会话返回 `session not created`，因为未启用 Allow remote automation。未擅自修改用户设置。 |
| Firefox | 当前未安装，未执行。 |

Chrome 实际结果：

- HTML 内联脚本及 HTTPS 静态脚本执行；SVG 图片、WAV 音频和 VP8 视频加载，音频/视频 `play()` 成功。
- 损坏视频显示下载回退；遗漏文件在时间线显示具体原因。
- guest 无法访问父 DOM、Cookie、localStorage；connect 和 worker 被 CSP 阻止，未产生被探测的 connect 请求。bootstrap 请求匿名，附件读取由父页面携带已认领会话。
- 未认领时附件直达拒绝；认领后同一浏览器续读，其他无 Cookie 页面返回 404。GET/HEAD/Range、撤销和到期还由 Tunnel 测试覆盖。
- 重复 ready 消息没有第二次交付。篡改 bootstrap 字节后 SRI 阻止执行，超时后显示下载回退。
- 在附件读取期间导航，父端取消读取、移除 iframe，无迟到交付。
- 公开时间线的公式、Mermaid、ECharts 和图片正确显示。Mermaid 最终 SVG style 携带 nonce；库内部测量阶段仍有被 CSP 拒绝的临时 style 控制台记录，没有放宽父页策略。

证据：[机器可读结果](validation/chrome-results.json)、[HTML 预览](validation/chrome-html.png)、[公开时间线](validation/chrome-timeline.png)。证据只保留脱敏结果和合成内容截图。

这轮浏览器验收覆盖真实 Tunnel 与最终 renderer，并非已运行整个 Electron 主会话 → Platform → Desktop → Tunnel 的人工操作链。完整四入口对照及多浏览器矩阵仍属于发布前验收。

## 5. 待验证项与发布步骤

1. 提供独立 MySQL 8.0 测试账号及 `TEST_MYSQL_*` 后执行 `make test-all`；不得使用生产数据库。
2. 由项目提供中性扫描名单 `FORBIDDEN_BRAND_TERMS` 后执行 `make verify-neutral`。
3. 在已允许自动化的 Safari 和安装 Firefox 的环境补齐 07 中的 HTML 隔离、一次性 Cookie、Blob 媒体、SRI/CORS、重放及导航矩阵。
4. 完成 Electron 主会话、应用内预览、公开分享、下载 HTML 的完整操作链对照；复核主应用能力保留和历史未冻结媒体降级。
5. 处理或逐项确认上表全仓失败项，达到项目发布门禁后才标记可发布。
6. 另获部署授权后，先发布兼容的新 Tunnel 及匹配 renderer/bootstrap，再发布配套 Platform/Desktop 与主 WebClient。使用合成会话复验，不回填旧快照；旧分享未保存的媒体需重建分享，旧下载 HTML 需重新导出。保留历史 hash；回滚前确认目标 Tunnel 接受已经创建的新合同。

## 6. 清理审计

已完成，结构化结果见 [cleanup-results.json](validation/cleanup-results.json)。

- 四仓 `git diff --check` 通过，根 workspace Git 状态干净；剩余工作区变更为本次源码、测试、文档及最终 Tunnel 渲染资产。未提交、推送或部署。
- 四仓 HEAD、分支、Git 配置及已有环境文件与任务前记录一致。没有留下临时 remote、refspec、worktree、代理、凭据或浏览器配置变化。
- 原先未跟踪的 8 份方案文档逐字节保留；9 个历史 assetSet 的 1,440 个文件 hash 全部一致。新 assetSet 的 161 个文件逐一通过长度、SHA-256 和 SRI 检查，只保留最终一组新增资产。
- Desktop 的 17 项生成路径恢复任务前状态，核对字节、权限和符号链接；Web 的原 dist/export/export-build、Webpack 缓存恢复，本次 diagnostics 和 export module graph 删除。
- 临时浏览器、测试服务器及挂起测试子进程已退出；最小浏览器探针 profile 无残留。浏览器测试 Go 入口、临时 SQLite/媒体/Cookie/profile、隔离基线副本、二进制、日志、缓存和恢复备份均已删除。
- 保留的验证截图和 JSON 为明确交付物，不包含真实分享 token、用户会话或凭据。没有未清理的临时状态。

本地忽略的构建目录已恢复原状，所以以后重新执行同步检查前，先在 WebClient 运行 `npm run build:conversation-export`，再运行 `npm run check:conversation-export-sync`。Tunnel 中本次交付的模板、manifest 和新资产不受该恢复影响。


## 7. 收尾边界修复（2026-10-20）

本轮按“非分享功能保持原有行为，稳定项目尽量少改”处理审查发现。未提交、推送或部署。

- Platform 只调整已有 `snapshot.go`、`resources.go` 两个生产文件：资源型 query、人类 steer 的扩展投影只在实际 snapshot 导出入口启用；普通消息读取、搜索和 Markdown 保留原有过滤规则。恢复 agent/team 的四份旧回放 golden，并新增同一输入在两类出口的行为对照测试。未复制投影器或增加服务层。
- WebClient 由应用宿主显式提供关联会话 href，恢复普通点击、修饰键点击和浏览器链接语义；公开分享不提供应用导航。自动化历史补齐宿主剪贴板和统一附件展示入口，保留生成图片的 artifact/reference 身份、加载重试和 Skill 激活。
- 复制反馈由单一位置负责：应用宿主保留 toast，公开分享使用局部状态。恢复关联会话原有样式，删除不再需要的按钮样式。
- 本轮 Desktop 的全部源码、测试和配置保持逐文件一致，没有新增生产抽象或修改稳定业务模块。

最终 renderer assetSet：`f9bdc1237af18ddd2a3449a9694cc4e274fdf4d2e0f12762e81735aa27a3fc9b`，161 个文件，runtime.js 1,241,557 字节。构建、真实模块边界、资产校验和同步检查通过。HTML 隔离 bootstrap 与首次浏览器验收版本逐字节一致；本轮未重跑第 4 节的完整浏览器矩阵。移除本任务尚未发布的两个过渡资产集，保留原有 9 组历史资产的全部 1,440 个文件。

验证：

- Platform：conversationexport、chatresource、conversation、server、chat 五包回归通过；投影/Control 相关竞态测试通过；最终主程序编译通过。分支条件整理后再次通过 conversationexport/conversation 回归。
- WebClient：主构建、类型、feature 边界、Desktop 契约、i18n 通过。全量 462 套件，首次 456 通过、6 失败；两套超时及连带失败套件单独复验 31/31 通过。其余 56 项为此前已在原 HEAD 复现的 4 套基线失败。本轮直接相关的卡片、自动化、预览、BTW、复制、选中文本和只读渲染测试 12 套、55 项通过，具体套件列于检查记录。
- Tunnel：匹配最终资产的 shareassets、desktop 两包 SQLite 回归通过。
- Desktop：原 HEAD 与当前源码分别在隔离副本构建主进程和 renderer；源码仓保持不变。补齐隔离副本的父级依赖和已有图标输入后复验，原生截图串行测试两侧均 5/5 通过，macOS 分享扩展两侧均 8/8 通过。服务管理测试先受沙箱本地端口限制，允许本地端口后仍触及整文件时限；按名称补跑剩余 18 项后，两侧全部 156 个命名用例均已覆盖或按平台条件跳过，剩余 3 处失败一致。按用例合并各轮最新结果，当前剩余 86 个失败均在原 HEAD 复现，没有当前版本独有的失败；这不等于全量测试通过。逐轮数量、失败名称和限制见 [boundary-fix-results.json](validation/boundary-fix-results.json)。

MySQL、中性扫描和尚未执行的完整人工链路仍按第 5 节记录，不把本次定向回归写成发布验收全部通过。

本轮清理审计通过：四仓 HEAD、main 分支和本地 Git 配置不变，最终差异均属于上述修复、测试、检查记录与匹配资产；Desktop 全部原有源码、测试和配置逐文件一致。Web 原 dist 的 1,005 个文件逐字节恢复，原方案文档与历史资产均保留。已确认本任务测试进程全部退出，并删除隔离副本、临时二进制、日志、缓存及合成测试目录。结构化清理结果一并保存在上述检查记录中。
