# Master Spec：Codex 必读索引

本文件不是新的需求来源，而是全套冻结文档的阅读顺序。若文档之间出现冲突，优先级如下：

面向人的分类导航见 `docs/README.md`；本文件只负责 Codex 的强制阅读顺序。

1. `AGENTS.md`
2. `docs/V1_FREEZE_SPEC.md`
3. `docs/DECISIONS.md`
4. `docs/MILESTONES.md` 的当前里程碑范围和验收标准
5. 当前里程碑的明确任务提示
6. 其他设计文档

`docs/DESIGN_REVIEW.md` 是 2026-07-18 的审查记录和导航，不是独立需求来源；发生差异时以上述优先级中的权威文件为准。

## 开发 Recorder Core 前必须阅读

- `PROJECT_CONTEXT.md`
- `V1_FREEZE_SPEC.md`
- `ARCHITECTURE.md`
- `DATA_MODEL.md`
- `API.md`
- `EVIDENCE_AND_INFERENCE.md`
- `OPERATIONS.md`
- `FAILURE_POLICY.md`
- `STABILITY_AND_FREEZE.md`
- `MILESTONES.md`
- `CODEX_WORKFLOW.md`
- `RISK_REGISTER.md`
- `DECISIONS.md`

设计审查完成后，还应阅读 `DESIGN_REVIEW.md`，了解已解决冲突、仍需在 M6 前落值的部署参数和审查限制。

## 设计扩展接口时必须了解，但不得在 Version 1 实现

- `LEARNING_ASSESSMENT.md`
- `QUESTION_QUALITY.md`
- `CAPTURE_SYSTEM.md`
- `VISION_PIPELINE.md`
- `WEARABLE_CAMERA.md`
- `MODEL_STRATEGY.md`
- `HEALTH_PSYCHOLOGY_DIET.md`
- `OPEN_QUESTIONS.md`

## Codex 行为要求

Codex 在开始任何里程碑前必须：

1. 说明已阅读哪些文件；
2. 复述当前里程碑范围；
3. 列出明确排除项；
4. 解释本次改动如何保持 record-only mode；
5. 不得因为后续智能能力需要而提前实现复杂基础设施。

M0-M6 的完整验收标准以 `MILESTONES.md` 为准；`codex-prompts/01_M0.txt` 至 `07_M6.txt` 只是任务入口，不得缩减验收项。设计审查完成不等于 M0 已开始或完成。
