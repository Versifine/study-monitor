# 文档导航

本目录同时包含 Version 1 权威规格、工程治理文档和 Version 1 之后的研究材料。文件保持平铺，避免移动路径导致任务提示和引用失效；通过本页区分用途。

## 当前状态

- Version 1 设计审查已完成
- M0 仓库与开发工具、M1 仅追加事件存储与本地 API、M2 外部媒体分段导入已完成
- M3 尚未开始，必须作为后续独立任务执行
- 时间不是当前最高风险；质量门槛优先于日历日期，M6 的 14 天认证不能压缩

## 三条阅读路径

### 你想快速确认项目方向

1. [`PROJECT_CONTEXT.md`](PROJECT_CONTEXT.md)
2. [`V1_FREEZE_SPEC.md`](V1_FREEZE_SPEC.md)
3. [`MILESTONES.md`](MILESTONES.md)
4. [`RISK_REGISTER.md`](RISK_REGISTER.md)

### Codex 准备执行一个里程碑

从 [`MASTER_SPEC.md`](MASTER_SPEC.md) 开始，按它列出的顺序完整阅读。任务入口位于 `codex-prompts/`，但不得缩减 `MILESTONES.md` 的验收标准。

### 你准备运维或冻结系统

1. [`OPERATIONS.md`](OPERATIONS.md)
2. [`FAILURE_POLICY.md`](FAILURE_POLICY.md)
3. [`STABILITY_AND_FREEZE.md`](STABILITY_AND_FREEZE.md)

## 文档分类

### Version 1 权威规格

| 文档 | 作用 |
|---|---|
| [`PROJECT_CONTEXT.md`](PROJECT_CONTEXT.md) | 用户目标、现实约束和产品定位 |
| [`V1_FREEZE_SPEC.md`](V1_FREEZE_SPEC.md) | Version 1 范围、模式和冻结完成线 |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | 单体架构、模块边界和建议目录 |
| [`DATA_MODEL.md`](DATA_MODEL.md) | 追加事实、投影、时间和版本规则 |
| [`API.md`](API.md) | M1/M2 本地事件、健康和媒体状态接口合同 |
| [`MEDIA_INGEST.md`](MEDIA_INGEST.md) | M2 外部媒体 sidecar、ready、受管存储、隔离和恢复合同 |
| [`EVIDENCE_AND_INFERENCE.md`](EVIDENCE_AND_INFERENCE.md) | Evidence/Observation/Inference 分层边界 |
| [`MILESTONES.md`](MILESTONES.md) | M0-M6 范围、排除项和验收标准 |

### 工程治理与运行

| 文档 | 作用 |
|---|---|
| [`MASTER_SPEC.md`](MASTER_SPEC.md) | Codex 必读顺序和冲突优先级 |
| [`DECISIONS.md`](DECISIONS.md) | 已冻结关键决策 |
| [`CODEX_WORKFLOW.md`](CODEX_WORKFLOW.md) | 单里程碑开发和 Git 检查点流程 |
| [`OPERATIONS.md`](OPERATIONS.md) | 正常运行、维护、备份恢复与回滚 |
| [`FAILURE_POLICY.md`](FAILURE_POLICY.md) | 故障级别与降级矩阵 |
| [`STABILITY_AND_FREEZE.md`](STABILITY_AND_FREEZE.md) | M6 稳定性认证规则 |
| [`RISK_REGISTER.md`](RISK_REGISTER.md) | 当前风险、缓解和退出条件 |
| [`DESIGN_REVIEW.md`](DESIGN_REVIEW.md) | 2026-07-18 审查记录，不是新需求来源 |

### Version 1 之后的研究材料

以下文档用于约束未来接口，不得成为 M0-M6 的实现范围：

- [`LEARNING_ASSESSMENT.md`](LEARNING_ASSESSMENT.md)
- [`QUESTION_QUALITY.md`](QUESTION_QUALITY.md)
- [`CAPTURE_SYSTEM.md`](CAPTURE_SYSTEM.md)
- [`VISION_PIPELINE.md`](VISION_PIPELINE.md)
- [`WEARABLE_CAMERA.md`](WEARABLE_CAMERA.md)
- [`MODEL_STRATEGY.md`](MODEL_STRATEGY.md)
- [`HEALTH_PSYCHOLOGY_DIET.md`](HEALTH_PSYCHOLOGY_DIET.md)
- [`OPEN_QUESTIONS.md`](OPEN_QUESTIONS.md)

## 现在不需要决定

- 模型供应商和提示词
- 纸面视觉算法
- 可穿戴硬件型号
- 原生手机应用
- 题库与主动测评方式
- 长期媒体降码率归档方案

这些决定都不能阻塞 Recorder Core。

## 下一步

1. 审查 M2 diff、测试、构建和 smoke 结果。
2. 为 M2 创建独立 Git 检查点并停止。
3. 只有在下一次明确任务中，才使用 `codex-prompts/04_M3.txt` 开始 M3。
4. 不要提前进入 ActivityWatch 适配、覆盖率、前端、运维自动化或 AI。
