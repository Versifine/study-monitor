# 使用 Codex 开发的约束流程

## 1. 原则

Codex 只能加快实现，不能替代范围控制。每次只做一个里程碑。

## 2. 初始化

```powershell
cd <repo>
git init
git branch -M main
codex
```

进入后先让 Codex 完整阅读：

- `AGENTS.md`
- `docs/MASTER_SPEC.md`
- `MASTER_SPEC.md` 在“开发 Recorder Core 前必须阅读”中列出的全部文档
- `docs/DESIGN_REVIEW.md`
- 当前里程碑任务提示

开始 M0 前必须先确认设计文档已经形成 Git 基线；没有可审查基线时不得把后续“审查 diff”视为已满足。

## 3. 每次任务模板

```text
先完整阅读 AGENTS.md 和相关 docs。
只实现当前里程碑，不进入下一里程碑。

开始修改前：
1. 复述范围和明确排除项；
2. 列出预计修改文件；
3. 指出可能扩大范围的风险。

完成后：
1. 运行测试和 smoke test；
2. 审查 diff；
3. 更新相关文档；
4. 列出剩余风险；
5. 停止，不自动继续。
```

## 4. Git 检查点

每个里程碑完成后：

```powershell
git status
git diff --stat
git add <本里程碑已审查文件>
git commit -m "feat: complete milestone Mx"
```

不要使用 `git add .` 把未审查文件或运行数据带入检查点。不要在多个未提交里程碑上继续开发。

## 5. 禁止行为

- 一次实现整个系统
- 在编码时继续探索产品目标
- 同时修改架构、数据模型和多个模块
- 因模型误判反复调提示词
- 在正式备考期间让 Codex自动修普通问题
- 用当前任务提示的简略文字缩减 `docs/MILESTONES.md` 的完整验收标准

## 6. 影子模式

所有智能能力先只输出预测，不改变正式计划。只有经过离线验证后才允许上线。

## 7. 备考期间

源码不要放在常用学习入口。运行构建产物；错误进入 issue 或日志，不自动弹出修复建议。
