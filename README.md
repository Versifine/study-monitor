# Exam Monitor

面向 2028 年考研准备的个人学习证据记录与评估系统。

本仓库的首要目标不是做一个“无所不知的 AI 助手”，而是在正式备考前构建并冻结一套稳定、低维护、不会诱导持续开发的基础设施：持续记录学习与生活证据，可靠保存原始数据，在不影响采集的前提下进行可选分析，并用主动测评补足被动观察无法判断的学习掌握度。

## 当前阶段

- 当前日期：2026-07-18
- 计划：2026 年 9 月开始进入持续备考节奏
- 当前状态：M0、M1 已完成；M2 尚未开始
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

当前已实现 M1 的 SQLite 仅追加事件存储与本地 API，不包含媒体、采集器导入、覆盖率、前端或 AI。

## 你现在应该做什么

先审查 M1 的测试、构建、smoke 和 Git 检查点，然后停止。只有在单独确认后，才把 [`codex-prompts/03_M2.txt`](codex-prompts/03_M2.txt) 作为下一次任务；不要提前进入 ActivityWatch、媒体、覆盖率、前端或 AI。

## M1 本地命令

要求 Windows PowerShell 5.1 和 Go 1.25.4：

```powershell
.\scripts\dev.ps1 -Version
.\scripts\dev.ps1 -Config .\configs\exam-monitor.example.json -CheckConfig
.\scripts\test.ps1
.\scripts\build.ps1
.\scripts\smoke.ps1
```

默认仅监听 `127.0.0.1:47831`。`GET /health/live` 只表示进程存活；`GET /health/ready` 检查 SQLite 是否可写；`POST /api/v1/events/batch` 接收版本化幂等事件；`GET /api/v1/events` 提供稳定快照分页。完整合同见 [`docs/API.md`](docs/API.md)。

生产默认数据目录稳定解析为 `%LOCALAPPDATA%\ExamMonitor\data`，数据库为其中的 `exam-monitor.db`，不依赖启动工作目录；`dev.ps1` 显式改用仓库中被 Git 忽略的 `data/`。绝对路径可以显式配置，相对路径只能在应用数据根内解析。构建产物写入被 Git 忽略的 `bin/`。SQLite 驱动与传递依赖固定在 `vendor/`，开发、测试和构建脚本使用本机 Go 工具链并关闭模块代理。

PowerShell 脚本在首次调用 Go 前强制使用本机已安装工具链，不允许 `GOTOOLCHAIN=auto` 静默下载；退出后恢复调用者原有环境。HTTP 连接的 header、读取、写入和空闲阶段均有配置超时。

配置优先级是安全默认值 → 可选 JSON 文件 → 环境变量。支持的环境变量：

- `EXAM_MONITOR_LISTEN_ADDRESS`
- `EXAM_MONITOR_ALLOW_NON_LOOPBACK`
- `EXAM_MONITOR_DATA_DIRECTORY`
- `EXAM_MONITOR_LOG_LEVEL`
- `EXAM_MONITOR_READ_HEADER_TIMEOUT`
- `EXAM_MONITOR_READ_TIMEOUT`
- `EXAM_MONITOR_WRITE_TIMEOUT`
- `EXAM_MONITOR_IDLE_TIMEOUT`
- `EXAM_MONITOR_SHUTDOWN_TIMEOUT`
- `EXAM_MONITOR_BUSY_TIMEOUT`
- `EXAM_MONITOR_MAX_OPEN_CONNECTIONS`
- `EXAM_MONITOR_MAX_REQUEST_BYTES`
- `EXAM_MONITOR_MAX_BATCH_EVENTS`
- `EXAM_MONITOR_MAX_EVENT_BYTES`
- `EXAM_MONITOR_MAX_PAYLOAD_DEPTH`
- `EXAM_MONITOR_MAX_CONCURRENT_WRITES`
- `EXAM_MONITOR_DEFAULT_PAGE_SIZE`
- `EXAM_MONITOR_MAX_PAGE_SIZE`
