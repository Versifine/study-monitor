# 文档导航

本目录同时包含 Version 1 权威规格、工程治理文档和 Version 1 之后的研究材料。文件保持平铺，避免移动路径导致任务提示和引用失效；通过本页区分用途。

## 当前状态

- Version 1 设计审查已完成
- Recorder Core 业务代码尚未开始
- 当前下一步是创建文档 Git 基线，然后只执行 M0
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

1. 人工确认 Version 1 范围和风险排序。
2. 将当前文档包创建为独立 Git 基线检查点。
3. 使用 `codex-prompts/01_M0.txt` 单独开始 M0。
4. M0 验收并创建检查点后停止，再决定是否开始 M1。

建议的文档基线命令：

```powershell
git status
git add .gitignore AGENTS.md README.md CHANGELOG.md docs codex-prompts
git diff --cached --stat
git diff --cached
git commit -m "docs: 冻结 Version 1 设计基线"
```

执行提交前先阅读暂存 diff；本页只是给出建议，不授权 Codex 自动提交。
