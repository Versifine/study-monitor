# Changelog

## Unreleased

- 修复 M1 严格 JSON schema：envelope 和事件根字段只接受大小写完全一致的正式名称，拒绝 `events`/`Events` 等可被 Go 结构体解码覆盖的别名。
- 修复 M1 审查问题：拒绝 envelope、事件和 payload 的重复 JSON key，拒绝未知 offset `-00:00`，并用提交前进程终止测试证明批次无部分残留且可安全重放。
- 完成 M1：SQLite WAL、只前向校验迁移、仅追加结构化 Evidence、幂等批量写入、稳定快照分页和存储就绪检查。
- 固定无 CGO SQLite 驱动及 vendor 依赖；Windows dev/test/build 在关闭模块代理时运行，M1 smoke 覆盖写入、重放、冲突、查询和重启恢复。
- 增加 M1 损坏输入、迁移失败、并发同键、事务回滚、数据库锁、异常退出和只读降级测试；未进入媒体、采集器、覆盖率、前端或 AI。
- 修复 M0 审查问题：未跟踪构建输入现在标记 dirty，Go 脚本禁止自动下载工具链，HTTP 连接生命周期有界，数据目录不再依赖进程工作目录。
- 完成 M0：Go 单体骨架、版本化 JSON 配置、结构化日志、构建版本、基础 liveness、Windows dev/test/build/smoke 脚本和确定性测试。
- 冻结项目目标、范围、架构、学习评估原则、故障策略和 Codex 开发流程。
- 完成 Version 1 设计一致性审查，明确受管媒体、仅追加事实与投影、覆盖率、降级、回滚和 M0-M6 验收边界。
- 增加面向人的文档导航和下一步入口；按“时间充裕、质量优先”重新排序 Version 1 风险。
- 完善 Git 忽略规则，覆盖本地 Evidence、SQLite 临时文件、构建/测试产物和本机配置。
