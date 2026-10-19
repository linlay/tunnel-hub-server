# Tunnel：分享准入

配置层定义两项 MiB 额度并换算为字节；`ShareLimits` 是上传入口与额度接口的唯一数据源。请求上限只推导，不增加第三项业务配置。

- `/api/desktop/shares/limits` 复用分享鉴权，返回两项字节值和 no-store。
- multipart 入口使用配置约束正文、资源累计与完整请求。正文/资源/请求分别返回可识别 413；读取到限额加一后停止，不声称已经完整测量 actualBytes。
- store 删除固定大小准入，继续做身份、版本、元数据、完整性与事务；历史迁移使用原数据长度/哈希，不应用新额度。
- 复用已有 MySQL packet 启动探测，由 Relay 传入所需容量；分享代理路由将大小判定交给 Go。
- 不改 staging 提交/补偿算法，不混入无关可靠性修复。

验证：SQLite 全量 `make test-sqlite` 通过；最终 desktop 包回归通过，涵盖 21 MiB 分享、调低新建额度后的历史读取，以及额度接口鉴权/no-store。Relay 构建通过。MySQL 实例未提供，集成验证未执行。
