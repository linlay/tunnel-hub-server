# Platform：导出出口检查

运行配置 `Export.MaxSnapshotBytes` 来自 `conversation-export.max-snapshot-mib`，默认 50 MiB。

`BuildSnapshotDocument` 保持原六参签名，只构建并序列化一次，不承担导出额度。普通消息、历史投影和对应测试不增加额度参数；三个仅因补参修改的测试文件已恢复原样。

HTTP 和工具各在实际导出出口对同一份 `document.JSON` 检查一次配置额度，随后直接返回 JSON 或从同一 Snapshot 生成 Markdown。工具移除重复构建，保持原空 Markdown 输出标题的语义；未配置工具额度明确失败。Service 在 App/Server 的真实组合边界接收配置，原 adapter 兜底不再加入额度赋值。

Snapshot 响应增加 X-Conversation-Export-Max-Bytes，并仅在该响应中追加 CORS 暴露头。通用 server_auth.go 完全恢复原样，其他接口不改变 CORS 响应。

本次修复复验：config、conversationexport、conversation、server 四包通过，主程序构建通过。已有测试直接覆盖空 Markdown、配置额度、缺失配置与非导出 CORS；配置用例断言准确字节换算。没有新增测试文件。
