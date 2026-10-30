# T3：共享渲染边界与错误降级

状态：待执行。责任仓库：`agent-webclient`。可以先独立推进；新增字段适配以 [01](01-solution.md) 为准。T5 负责公开附件的具体预览实现。

## 1. 责任与改动入口

保留现共享组件链，不重建统一 ViewModel。T3 负责让相同内容在四个入口安全展示，主应用操作由宿主注入。

| 现有入口 | 修改目的 |
| --- | --- |
| [TimelineInteractionContext.tsx](../../../agent-webclient/src/features/timeline/components/TimelineInteractionContext.tsx) | 复用现能力合同，缺失能力保持缺失 |
| `src/features/timeline/components/ConnectedConversationStage.tsx`、`src/features/conversation/components/ChatPreviewSurface.tsx` | 宿主承接认证资源、打开、下载、菜单等动作 |
| `src/features/timeline/components/ImageGenerationCard.tsx`、`src/features/artifacts/components/ReferenceCard.tsx`、`src/features/timeline/components/planning/index.tsx` | 展示组件解除主应用依赖 |
| `src/features/timeline/components/RelatedChatCards.tsx`、`TimelineRow.tsx` | 无能力时取消误导性跳转/按钮 |
| [conversationSnapshotV1.ts](../../../agent-webclient/src/export/conversationSnapshotV1.ts)、`src/export/index.tsx` | 安全字段投影、页面边界与旧快照兼容 |
| `src/shared/ui/ConversationMarkdown.tsx`、`src/shared/ui/markdown-code/MarkdownECharts.tsx`、`ConversationMarkdownCode.tsx` | 异步加载、图表与复制失败反馈 |
| `scripts/check-conversation-export-template.js`、`webpack.export.config.js` | 真实构建依赖门禁 |

遵守本仓 `AGENTS.md` 和既有 React 工程约定；本功能复用专用资源/导出客户端，不为形式统一替换成另一类业务 HTTP client。

## 2. 实施步骤

1. 列出导出入口实际可达的运行时依赖，覆盖成功 image_generate、Reference、Planning 等路径。type-only 依赖不等于运行时耦合，不能凭文件名误判。
2. 图片生成卡保留结果解析、布局、loading/error 展示；其认证 URL、主应用下载、上下文菜单和打开行为移回宿主，通过现 `renderAttachment` 等能力供给。保留生成结果的 artifact/reference/resource 身份、加载后打开及失败重载行为；普通 AttachmentCard 的 `id` 当前可能被当作 referenceId，不能把快照资源 ID 直接塞入，也不能直接替换后声称行为等价。复用既有附件展示选项和原 Tile 行为，不为每种格式新增 context 方法。
3. ReferenceCard 显式消费打开动作；主会话、Composer、应用内预览调用方负责注入。删除内部无条件 `useOpenTarget` 及兜底寻找主应用上下文的分支。
4. Planning 打开按钮同时要求目标 ID 和真实回调；复制反馈使用可独立工作的局部状态或现局部消息 holder，不依赖导出根不存在的 `<AntdApp>`。
5. 无 openSource/openChat/openTarget 等能力时，隐藏或禁用相应动作。删除空函数伪装能力、`/?chatId=` 等在公开 origin 下错误的导航回退。
6. 按 01 显式解析 V1 已支持字段及新增字段；保留 `size/sha256/mediaRefs`，把 `attachmentIds` 解析成成功文件或省略占位，再投影为现 TimelineNode。不要把未验证原节点整体展开到渲染状态。
7. 为原公式 import Promise、图表 import/init/setOption、复制操作补上相应失败处理与卸载保护。预期失败只影响当前块；失败加载缓存可清除，禁止引入自动重试调度系统。
8. 在导出 React 根增加一个页面级 ErrorBoundary。保留现同步启动/无效 JSON 错误页面，两者覆盖的阶段不同，不叠加多个页面级边界。
9. 升级已有构建检查：检查入口真实模块依赖和实际生成的关联资源，覆盖间接/异步依赖，禁止 AppContext、认证请求、Desktop runtime 等主应用能力进入只读包。保留现资源大小和 chunk 约束，不为绕过检查扩大预算。

## 3. 删除清单

- 共享组件内 `useOpenTarget/useApp/useAppContext` 等主应用能力回退，以及只为这些回退存在的 import。
- 图片渲染中自行猜 chatId、拼认证 URL、调用运行时的备用路径。
- 只读入口的 no-op 能力注入、错误主站链接、永远不可用的“打开”按钮。
- 被替代的对象透传 cast、未处理 Promise 和重复错误兜底。
- 仅靠固定源码文件白名单证明“导出包无运行时耦合”的旧检查结论；可保留有实际价值的快速检查，但真实构建图才是最终门禁。

不新增分享专用卡片副本、完整 AppProvider 包装、统一宿主枚举或 Context 注册系统。

## 4. 验收与测试

优先扩展既有 ImageGenerationCard、ReferenceCard、Planning、TimelineInteractionContext、ConversationPreview、ConversationMarkdown 和 MarkdownECharts 测试。使用真实卡片在最小只读根中的渲染回归，不能只 mock 掉曾触发问题的组件。

| 用例 | 主会话 | 应用内预览 | 公开分享 / 下载 HTML |
| --- | --- | --- | --- |
| 成功图片生成与失败结果 | 原图、打开、菜单行为保持 | 按宿主能力展示 | 不要求 AppProvider；分享图片由 T5 提供，本地 HTML 无媒体包时可读降级 |
| Reference / Planning | 原操作可用 | 真实能力可用 | 无能力无死按钮，复制结果可见 |
| 公式/图表加载或解析失败 | 当前块降级 | 当前块降级 | 页面其余内容保持 |
| 复制失败、组件卸载后返回 | 局部反馈，无迟到更新 | 同左 | 同左 |
| 未预期 React 渲染异常 | 沿用应用边界 | 沿用应用边界 | 页面级可见失败态 |
| 等待继续、工具 VIEW、终端、语音 | 原行为不回退 | 保持原只读约束 | 不恢复实时或写入行为 |

运行 `npm run typecheck`、`npm run check:boundaries`、`npm run check:desktop-contract`、`npm run check:i18n`、相关 `npm run test:unit -- ...` 和 `npm run build:conversation-export`。现 `check:conversation-export` 还校验 sibling Tunnel 已同步，因此其完整通过留到 T6 同步之后；T3 交付本地构建和依赖检查结果，不为绕过同步检查临时改路径或增加平行脚本。本任务不直接同步 Tunnel；T6 在 T5 完成后统一发布同一次构建产物。

## 5. 交付与清理

与 T5 提前约定 `conversationSnapshotV1.ts`、`ConversationExportDocument.tsx` 和构建脚本的编辑责任：T3 提供安全模型/边界，T5 增加实际媒体视图和 HTML bootstrap，避免各自重写同一解析器。

交付四入口回归证据、构建依赖检查结果和被删除的运行时耦合清单。清理仅限本任务生成且不需交付的测试/预览文件；不清理历史 Tunnel 资产，也不改用户环境配置。

执行记录（实施后填写）：实际变更；删除项；验证结果；未完成项；清理结果。
