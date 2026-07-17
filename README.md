# Exam Monitor

面向 2028 年考研准备的个人学习证据记录与评估系统。

本仓库的首要目标不是做一个“无所不知的 AI 助手”，而是在正式备考前构建并冻结一套稳定、低维护、不会诱导持续开发的基础设施：持续记录学习与生活证据，可靠保存原始数据，在不影响采集的前提下进行可选分析，并用主动测评补足被动观察无法判断的学习掌握度。

## 当前阶段

- 当前日期：2026-07-18
- 计划：2026 年 9 月开始进入持续备考节奏
- 当前状态：M0（仓库与开发工具）已完成，M1 尚未开始
- 冻结目标：Recorder Core 通过 M0-M6 后才成为正式依赖，不为赶日期压缩验收
- 正式备考期间：只允许重启、回滚、关闭故障模块，不进行研究性调参和功能开发

## 先读这些文件

1. [`AGENTS.md`](AGENTS.md)：Codex 必须遵守的仓库级规则
2. [`docs/README.md`](docs/README.md)：面向人的文档分类和阅读路径
3. [`docs/MASTER_SPEC.md`](docs/MASTER_SPEC.md)：Codex 必读顺序和冲突优先级

## 核心原则

优先级必须固定为：

1. 不干扰学习
2. 不丢失原始证据
3. 自动恢复
4. 可回滚、可关闭
5. 明确显示数据覆盖率和系统健康
6. 分析准确度
7. 功能数量和视觉设计

任何低优先级能力不得损害高优先级目标。

## 仓库建议结构

Version 1 的完整建议目录和模块边界见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)。不预建分析、模型或微服务目录。

当前只实现 M0 基础骨架，不包含事件写入、SQLite、媒体、采集器、前端或 AI。

## 你现在应该做什么

先检查 M0 的测试、构建和 smoke 结果，然后停止。只有在单独确认后，才把 [`codex-prompts/02_M1.txt`](codex-prompts/02_M1.txt) 作为下一次任务；不要同时研究媒体、采集器或 AI。

## M0 本地命令

要求 Windows PowerShell 5.1 和 Go 1.25.4：

```powershell
.\scripts\dev.ps1 -Version
.\scripts\dev.ps1 -Config .\configs\exam-monitor.example.json -CheckConfig
.\scripts\test.ps1
.\scripts\build.ps1
.\scripts\smoke.ps1
```

默认仅监听 `127.0.0.1:47831`，运行数据目录默认为被 Git 忽略的 `data/`，只提供不依赖数据库的 `GET /health/live`。M0 不创建数据目录或写入业务数据。构建产物写入被 Git 忽略的 `bin/`。M0 只使用 Go 标准库，因此 `go.sum` 当前为空。

配置优先级是安全默认值 → 可选 JSON 文件 → 环境变量。支持的环境变量：

- `EXAM_MONITOR_LISTEN_ADDRESS`
- `EXAM_MONITOR_ALLOW_NON_LOOPBACK`
- `EXAM_MONITOR_DATA_DIRECTORY`
- `EXAM_MONITOR_LOG_LEVEL`
- `EXAM_MONITOR_READ_HEADER_TIMEOUT`
- `EXAM_MONITOR_SHUTDOWN_TIMEOUT`
