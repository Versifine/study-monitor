# Changelog

## Unreleased

- 修复 M0 审查问题：未跟踪构建输入现在标记 dirty，Go 脚本禁止自动下载工具链，HTTP 连接生命周期有界，数据目录不再依赖进程工作目录。
- 完成 M0：Go 单体骨架、版本化 JSON 配置、结构化日志、构建版本、基础 liveness、Windows dev/test/build/smoke 脚本和确定性测试。
- 冻结项目目标、范围、架构、学习评估原则、故障策略和 Codex 开发流程。
- 完成 Version 1 设计一致性审查，明确受管媒体、仅追加事实与投影、覆盖率、降级、回滚和 M0-M6 验收边界。
- 增加面向人的文档导航和下一步入口；按“时间充裕、质量优先”重新排序 Version 1 风险。
- 完善 Git 忽略规则，覆盖本地 Evidence、SQLite 临时文件、构建/测试产物和本机配置。
- 尚未实现 Recorder Core 业务代码。
