# T2：Desktop 冻结、上传与逐项省略反馈

状态：待执行。责任仓库：`zenmind-desktop`。前置：T1 的候选合同和读取入口；T4 可用后完成真实上传联调。技术合同以 [01](01-solution.md) 为准。

## 1. 责任与改动入口

Desktop 只消费清单并冻结文件，不解析 Markdown、HTML 或工具输出找资源。复用现 worker、controller、Tunnel client 与分享弹窗。

| 现有入口 | 修改目的 |
| --- | --- |
| [html-worker.ts](../../../zenmind-desktop/src/main/modules/conversation-share/html-worker.ts) | 有界读取、完整性校验、预算选择、最终快照 |
| `src/main/modules/conversation-share/export-contract.ts`、`html-render-service.ts` | 传递同一份省略结果，保留新增字段 |
| `src/main/modules/conversation-share/controller.ts`、`tunnel-client.ts` | 上传保持原契约；返回创建结果和逐项原因 |
| `src/shared/contracts/copilot.ts` | 结果契约的唯一源定义 |
| `src/renderer/app-shell/conversation-share/useConversationShareDialog.ts`、`ConversationShareDialog.tsx` | 在现结果区呈现文件名/原因 |

按本仓 `AGENTS.md` 先读 Desktop 的架构、导航/鉴权、桌面协议相关文档。共享契约通过现生成脚本同步，禁止手改生成文件。

## 2. 实施步骤

1. 保留 `kind: "html"` 与 `kind: "snapshot"` 两条现有业务路径：本地 HTML 只修渲染，本任务只对分享 snapshot 冻结资源。
2. 每次创建读取 Tunnel limits，沿用 Platform 响应头和现有有界读取器。不得在 Desktop/UI 增加固定 50 MiB 或新文件数限制，不重复上一轮额度重构。
3. worker 按候选稳定顺序调用 T1 的受限读取入口。只接受 loopback Platform、既有 bearer 认证和当前 `chatId/sourceRef`，不读取任意绝对路径、不跟随外部资源清单下载 CDN。
4. 读取成功时核验实际 MIME、size，以及存在的预期 hash；计算最终 SHA-256。成功结果以实际字节构造 multipart 描述符，仍只保留一份供上传的字节。
5. 区分单项失败原因；现 `fetchLimitedResponse` 会把 404、409、其他失败和部分超时合并，必须在资源读取路径保留 01 约定的状态/超时原因，不能只修改外层 catch。快照/模板整体失败行为保持，不为此新建通用错误框架。保留 Platform 已提供的省略项，再合并冻结阶段结果；成功/省略 ID 不相交，重复资源不重复读取或上报。
6. 总时限到达后为每个未处理候选记录 `timeout`。资源预算不足记录 `too_large`；文件变化、读取拒绝、消失分别保留原因，不能全部折成“附件已省略”。
7. 重建附件 JSON 时显式保留 `mediaRefs`；正文保留 `attachmentIds`。最终清单只含成功冻结项，失败项进入 `omittedResources`，所有关系仍可解析为成功或省略。
8. 保持正文只序列化一次；将附件与省略清单一并纳入最终元数据预算。有限次移除末尾成功项并补原因仍超额时，整次失败；禁止静默裁剪原因、悬空引用或重复编码整份大正文来试探额度。
9. worker → render service → controller → shared contract 使用同一来源的逐项结果。UI 展示文件名和本地化原因；已有简短 warning 可由清单派生，不再独立维护真假状态。
10. 保留 snapshot 第一 part、`attachment:<id>` 后续 part、上传失败映射和原子创建。上传失败不能返回一个不可用的“成功链接”；不追加重试队列或失败后补传。

## 3. 必须删除或替换

- `attachmentsOmitted`、`attachmentUnauthorized` 两个布尔值作为独立事实来源的路径，改为由省略清单推导展示。
- 只保留六个附件字段而丢失 `mediaRefs` 的重建代码。
- 超时直接退出而没有剩余文件诊断的分支。
- 额度裁剪后不补原因或留下悬空映射的分支。
- 旧 artifact-only 读取地址在分享 worker 中的使用；其他业务使用旧接口不受本任务影响。

## 4. 验收与测试

优先扩展 `test/conversation-html-worker.test.mjs`、`conversation-share-controller.test.mjs`、`tunnel-conversation-share-client.test.mjs`、`conversation-share-dialog-hook.test.mjs` 和已有 worker/导出测试。

- [ ] 全部成功：上传字节/hash/size 与最终附件严格一致，HTML 映射完整。
- [ ] 普通媒体候选未提供 hash 时成功冻结并计算最终 hash；提供空/非法 hash 时按错误候选处理，不能把二者混为一类。
- [ ] 混合缺失、拒绝、超额、变化、超时：正文成功，每项省略有名称和原因，成功文件不重复上传。
- [ ] 所有资源均省略：仍可生成合法正文分享；节点和 HTML 的缺失引用有反馈。
- [ ] 正文加诊断超过快照额度：明确失败，不静默删除诊断；边界值使用 Tunnel 响应额度。
- [ ] HTML 本身省略、HTML 媒体省略、重复引用、取消/worker 失败都不留下错误映射或多余缓存。
- [ ] 契约和弹窗展示同一份结果，无绝对路径、token 或原始错误泄漏。
- [ ] 原本地 HTML 导出仍可保存，未新增离线附件包；其独立额度语义保持。

执行本仓实际命令：`npm run architecture:check`、`npm run agent-webclient-contract:check`、`npm run i18n:check`、类型/主进程构建及相关测试。修改契约时先使用 `npm run agent-webclient-contract:generate` 再检查，并交接 WebClient 的生成产物。完整 `npm test` 按本仓要求在集成阶段运行；构建脚本可能生成品牌/版本资源，执行前记录其状态，不把无关变化混入提交。

## 5. 交付与清理

向 T5 提供真实部分成功结果，向 T6 提供 macOS/Windows 相关差异和兼容版本要求。新 Desktop 不能先于支持新 sourceRef 的 Tunnel 上线，不能假定旧 Platform 支持新读取接口。

清理本任务生成的导出文件、临时资源、测试服务和 worker；保留用户既有文件、缓存和配置。检查 Git diff 中只有任务源码、必要契约生成结果及相关测试/文档，无品牌、版本号或 `.env` 的意外变化。

执行记录（实施后填写）：实际变更；删除项；验证结果；未完成项；清理结果。
