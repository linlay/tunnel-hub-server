# T1：Platform 快照投影与受限资源读取

状态：待执行。责任仓库：`agent-platform`。前置：[01 最终方案](01-solution.md) 已确认。输出供 T2 冻结、T3/T5 展示使用。

## 1. 责任与改动入口

本任务只负责选定历史的投影、资源发现与元数据、当前 Chat 文件读取。不得处理 Tunnel 预算、公开 URL、浏览器预览或后台同步。

| 现有入口 | 修改目的 |
| --- | --- |
| [snapshot.go](../../../agent-platform/internal/conversationexport/snapshot.go)、`attachments.go` | 在原投影中保留文件引用，建立规范候选 ID 与节点关系 |
| [handler_conversation_export.go](../../../agent-platform/internal/server/handler_conversation_export.go)、`server_routes.go` | 导出端补充元数据，注册薄的资源读取接口 |
| `internal/conversation/control.go` | 检查现工具导出调用，使用同一投影/导出规则，不复制一套发现逻辑 |
| `internal/chatresource/artifacts.go` | 复用已发布 artifact 的 manifest 与完整性检查 |
| `internal/chat/resource_key.go`、`internal/chat/lifecycle.go`、`internal/documentmeta/metadata.go` | 复用路径、符号链接、文件和 MIME 判定 |

先读本仓 `AGENTS.md`。只在当前导出领域内整理必要函数，不新建跨应用资源服务或通用依赖图。

## 2. 实施步骤

1. 按 01 第 4 节补充可选 `attachmentIds/mediaRefs/omittedResources`。保持 V1，沿用规范引用的稳定 ID；不得用全路径、随机临时 ID 或另建媒体模型。
2. 修复仅含文件的 query 被空文本过滤的问题；保留人类 steer 的可见正文/附件，不再因没有 agent origin 丢失它。保留现有 agent/team 来源语义，不把内部事件放入公开快照。
3. 在现时间线投影时读取明确的文件引用、实际媒体输出和 Markdown 媒体节点。结构化工具结果在转成 `resultText` 之前收集，避免序列化后再盲扫所有字符串。
4. 已发布 artifact 清单仍是正式附件来源；把实际输出引用的当前 Chat 媒体追加并去重。纯投影不打开文件。导出端随后使用现有 Chat/资源能力补元数据，最后序列化一次；不要先构建整份 JSON、再解析、再构建另一份快照。
5. 对候选 HTML 使用现有 HTML 解析能力（仓库已有 `golang.org/x/net/html`）收集 01 约定的静态媒体属性，形成 `mediaRefs`。本阶段不执行脚本、不请求外网；重复引用共享一个资源 ID。
6. 相对引用按 HTML 当前 Chat 发布目录解析；`@chat/` 按 Chat 根解析，再进入统一规范化。拒绝越界、内部路径、绝对文件路径、未知 scheme 和跨 Chat 引用。原 Workspace 发布目录已丢失，不能尝试回溯。
7. 对可规范化但缺失/不允许的文件生成有 ID 的省略项；无法规范化的候选只保留安全名称与原因。已发布清单读取失败不能只写日志后伪装所有附件正常，须在响应中给出可见的安全诊断；核心历史读取失败仍使导出失败。
8. 增加 `POST /api/chat/export/resources/read`，仅接收 `chatId/sourceRef`。复用 Desktop app/device 认证；published 文件走 `OpenArtifactByRef`，普通文件经既有安全解析后只读媒体。返回文件字节与规范 MIME，沿用请求取消和关闭文件语义。
9. 按 01 的状态/原因映射返回实际错误，供 Desktop 区分不存在、拒绝、变化、超时和读取失败；例如 404 是 `not_found`、409 是 `changed`，不能统一返回一个不可区分的读取失败。普通媒体不预先全量读取做 hash；现 `AttachmentV1.SHA256` 改为 `omitempty`，缺省不输出空字符串；已有 manifest hash 不丢弃。新候选 size 必须来自 stat，stat 失败记省略，不能把未知写成零。
10. HTTP 与工具导出的真正快照出口遵守同一规则；内部 Markdown/对话控制投影不因这次修改额外读所有附件。不扩展 Markdown 导出为媒体包。

## 3. 必须删除或替换

- 只统计 skipped 数量、对用户无逐项原因的本任务相关附件丢弃路径。
- “空文本等于没有消息”的文件消息过滤条件。
- 为导出方便放宽现 `artifacts/read` 或直接复用整个 `/api/resource` handler 的尝试。
- Desktop/Tunnel 二次发现资源的配套草案；发现规则只在本仓。
- 被新投影替代的独立事件遍历或重复序列化；不要保留一新一旧两套候选生成器。

## 4. 验收与有意义的测试

在现 `internal/conversationexport/*_test.go`、`internal/server/handler_conversation_export_test.go` 及资源相关测试中补覆盖；新 POST handler 可以有对应的聚焦测试，不增加泛化测试框架。

- [ ] 无附件旧会话输出仍合法；原发布附件的 ID/顺序及 size/hash 保持。
- [ ] 只有图片的 query、只有音频/视频的 steer、成功 image_generate 都能进入候选和相应节点；失败生成结果不凭空创建资源。
- [ ] 相同资源经节点、Markdown、HTML 多处引用只产生一个候选；普通工具输入路径不会被扫描。
- [ ] HTML 同目录媒体、`@chat/`、编码文件名、`srcset`、poster、丢失媒体有确定映射和原因；外部 HTTPS 保留且服务端没有发起请求。
- [ ] 当前 Chat 普通媒体可读；非媒体未发布文件、越界路径、跨 Chat、绝对路径、内部目录和越界符号链接被拒绝。
- [ ] app/device 身份有效性由现中间件保障；缺少身份、伪造字段或错误 runtime 不能利用新入口。
- [ ] 已发布文件在发布后被修改会拒绝读取并映射为 `changed`；普通文件响应有规范 MIME。
- [ ] 普通媒体候选 JSON 没有 `sha256` 而非 `sha256: ""`，size 为真实 stat 值；该候选可被 Desktop 成功冻结并补齐 hash。
- [ ] HTTP/工具快照出口一致，正文只序列化一次；原独立导出额度和响应头仍有效。

至少运行涉及的 `internal/conversationexport`、`internal/server`、`internal/chatresource` 测试以及本仓要求的构建/检查；按实际修改范围选择测试，不把未配置的集成依赖记录为通过。路径校验应包含 macOS/Windows 风格输入，不能以开发机平台代替拒绝语义。

## 5. 交付与清理

交付新字段样例、成功/部分成功样例、资源错误映射和实际测试命令，交给 T2/T4/T5。不要把候选资源可读性误写为已经冻结。

执行前后记录 Git 状态；检查只修改本任务源码和必要测试/文档。删除本任务创建的临时 Chat、文件和服务，归属不明的保留并报告。不得修改真实配置、历史数据库或既有用户文件。

执行记录（实施后填写）：实际变更；删除项；验证结果；未完成项；清理结果。
