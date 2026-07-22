# 运维手册

## 1. 正常运行原则

系统平时不要求用户操作。每日状态只显示事实和覆盖缺口，只有核心风险才通知。生产路径是当前用户的 Windows Task Scheduler 任务：登录后启动，非正常退出后按任务设置有界重启。生产默认数据根是 `%LOCALAPPDATA%\ExamMonitor`，不依赖任务的工作目录；其他卷必须用绝对路径显式配置。Docker、WSL、开发服务器、管理员权限和云服务都不是正常运行依赖。

## 2. 固定故障操作顺序

1. 受监督进程按有界退避自动重启模块或核心服务
2. 仍失败则用一条命令回滚上一稳定应用版本
3. schema 不兼容或回滚仍失败时，关闭故障的可选模块并进入 Minimum mode
4. 保留故障记录，在固定维护窗口处理；超过窗口仍未恢复则保持降级到考研后

正式备考期间没有“打开代码现场调试”步骤。任何脚本失败都必须非破坏性停止并给出稳定错误码，不得自动尝试未知修复。

## 3. 通知门槛

只在以下情况通知：

- 核心采集超过冻结清单的离线时间
- 磁盘进入关键水位或数据库保留空间受威胁
- 数据库完整性失败、受管存储不可读或备份连续超过 RPO
- 外部采集器明确上报视频持续失焦/遮挡；Version 1 不自行运行视觉模型判断
- 软件严重影响设备性能或进入崩溃循环

AI 分析未安装/关闭、分析积压、分类错误、周报延迟和 UI 体验问题不即时通知。`unknown` 覆盖状态只有超过对应核心采集器 SLA 后才升级为 P1。

## 4. 维护窗口

建议每周固定一次，最多 30 分钟。

维护窗口只允许检查状态、重启、验证备份、回滚或关闭模块。超时仍未恢复：回滚或停用模块，不继续调试。

## 5. 模式切换

- Record-only mode 是正常冻结运行路径，分析模块保持关闭或未安装
- Minimum mode 只启用 ActivityWatch、书桌媒体、事件存储、健康检查和备份
- 模式切换必须记录操作者/触发器、时间、旧模式、新模式和原因
- 模式切换不得重写 Evidence；重新启用模块后按幂等水位继续

## 6. 磁盘保护

- 预警水位：暂停低优先级导入，关闭可选分析，发出非打扰式状态
- 关键水位：拒绝新媒体，继续写小型核心事件和故障记录，停止所有媒体删除之外的高占用工作
- 保留空间受威胁：数据库进入保护性降级，不确认无法持久化的写入
- 自动保留默认关闭；开启后也只处理受管、已接受、超过最短保留期且满足备份条件的媒体本体
- Version 1 不自动删除结构化事件、心跳、故障、媒体元数据或状态历史
- 不删除未知、写入中、隔离中、来源未确认或不在 Recorder Core 管理目录的文件
- 日志按大小和保留份数轮转；只清理超过恢复窗口且已由导入事实确认无用的 Core 临时文件，不能用“临时目录”名义删除来源文件

具体水位在 M6 冻结清单中记录，认证期间不得放宽。

M4 安全默认值为：预警 `10 GiB`、关键 `5 GiB`、数据库保留空间 `1 GiB`，并强制满足 `预警 > 关键 > 保留空间 >= 64 MiB`。后台每 30 秒检查并持久化状态转换；此外 HTTP/ActivityWatch 的每次实际提交和媒体每个 1 MiB 复制块/原子改名前都会同步调用系统可用空间探针，不复读后台缓存，因而最坏单次媒体在途写入远小于最小保留空间。测试通过可变探针覆盖运行中从 normal 切到 reserve，不会真的填满磁盘。`/api/v1/operations/status` 返回最近一次（包括提交门控触发的）水位、可用字节、错误码、检查时间和保留模块状态。保留扫描只读取并校验完整备份 manifest 后建立元数据索引，再查询最多 `max_deletes_per_run` 个候选；只对这些候选对应的备份本体和受管源文件做大小/SHA-256 校验，不会在每轮扫描重哈希整份备份。

日志写入 `<data>\logs\exam-monitor.jsonl`，默认每份 `10 MiB`、保留 5 份。WAL 默认每 5 分钟 checkpoint；超过 `64 MiB` 时请求 `TRUNCATE`。临时清理只识别 `<data>\media\staging` 内、名称为 64 位十六进制加 `.partial`、超过 24 小时且不被最新导入事实引用的普通文件；不扫描或删除来源入口、隔离区和未知文件。

### 只读仪表盘

Record-only 默认可在本机监听地址打开 `/`。页面显示六类事实状态但不自动轮询、不提供任何修改按钮；正常运行不需要 Node.js 或前端开发服务器。`EXAM_MONITOR_DASHBOARD_ENABLED=false` 可独立关闭，Minimum mode 始终关闭。关闭后根页面和 `/api/v1/dashboard/summary` 返回 404，但健康检查、写入、采集、恢复、备份和回滚保持原合同。

若页面显示“未知”“无数据”“未上报”“已关闭”或“未安装”，不得人工改成零或正常；先按对应采集器 SLA 与稳定错误码判断是否需要维护。分析未安装是正常 Record-only 状态，不触发通知。仪表盘资源校验失败会把 dashboard 模块记录为 `unavailable/DASHBOARD_ASSETS_INVALID` 并继续 Recorder Core；不要在正式学习时现场修前端。

## 7. 备份与恢复

备份分为：

- 元数据备份：一致的数据库快照、配置、构建/schema 版本、媒体清单、校验和、恢复脚本和覆盖范围声明
- 完整 Evidence 备份：元数据备份加清单范围内的媒体本体

元数据备份不等同于媒体灾难恢复，界面和报告必须明确标注覆盖范围。备份成功只在所有校验通过后确认。

恢复默认写入新目录：先拒绝绝对路径、双斜杠穿越、reparse point 和重复目标，复制每个文件后立即复核大小与 SHA-256，再运行数据库完整性、四个迁移账本和媒体清单校验。完整备份恢复以快照数据库中的 accepted/restored 清单为权威，与主 manifest、`metadata/media-manifest.json` 和恢复后的每个媒体本体逐项交叉验证；删掉清单条目不能把缺失 Evidence 伪装成完整恢复。默认不得覆盖或切换当前数据目录。M6 必须从实际备份演练完整恢复，并用抽样回放或字节校验证明媒体可用。

实际命令（所有路径使用绝对路径）：

```powershell
# 元数据备份；不包含媒体本体，manifest 会明确 included=false
.\scripts\backup.ps1 -BinaryPath C:\ExamMonitor\exam-monitor.exe -ConfigPath C:\ExamMonitor\exam-monitor.json -DestinationDirectory E:\ExamMonitorBackups -Type metadata

# 完整 Evidence 备份；只有全部哈希验证通过才更新 data\backup\latest-full.json
.\scripts\backup.ps1 -BinaryPath C:\ExamMonitor\exam-monitor.exe -ConfigPath C:\ExamMonitor\exam-monitor.json -DestinationDirectory E:\ExamMonitorBackups -Type full

# 默认/推荐恢复到一个不存在的新目录，不切换当前数据根
.\scripts\restore.ps1 -BackupDirectory E:\ExamMonitorBackups\exam-monitor-... -VerifierBinaryPath C:\ExamMonitor\exam-monitor.exe -TargetDirectory E:\ExamMonitorRestored
```

数据库快照由核心二进制使用 SQLite `VACUUM INTO` 生成，随后只读执行 `PRAGMA integrity_check`，并按内嵌迁移逐项核对四个账本的版本、名称、顺序和 SHA-256。manifest 最后写入，列出备份类型、覆盖声明、数据库/配置/构建/schema/恢复脚本和每个受管媒体的大小、SHA-256 与是否包含本体。失败和中断不会替换上一份完整备份标记。`-ConfirmOverwrite` 只允许显式处理已存在恢复目录，并先把旧目录移动为可恢复的 `.pre-restore-*`，不直接删除。

## 8. 应用回滚

每个稳定发布目录包含二进制、静态前端、默认配置、迁移兼容范围和构建清单。回滚脚本：

1. 检查上一稳定版本和当前 schema 是否兼容
2. 停止当前受监督进程
3. 原子切换应用版本和兼容配置
4. 启动并运行 smoke/健康检查
5. 失败时保留当前 Evidence 和两个应用版本，进入安全降级

回滚不删除数据、不执行 down migration、不恢复旧数据库快照。数据恢复是独立且显式的操作。

构建、安装、回滚和卸载命令：

```powershell
$binary = (.\scripts\build.ps1 -OutputDirectory C:\ExamMonitorBuild | Select-Object -Last 1)
.\scripts\install.ps1 -BinaryPath $binary -ConfigPath C:\ExamMonitor\exam-monitor.json
.\scripts\rollback.ps1
.\scripts\uninstall.ps1
```

`build.ps1` 同目录生成 `release-manifest.json`，记录二进制 SHA-256、版本/commit/build time、配置 schema 和 core/media/M3/M4 数据库兼容范围。`install.ps1` 先用待安装二进制执行 `--version`/`--check-config`，拒绝发布路径逃逸、同一版本号下不同二进制/manifest/配置，以及升级时改变规范化数据库路径（切换数据根只能走显式恢复流程）；随后结束旧任务和受管子进程，在 `%LOCALAPPDATA%\ExamMonitor\releases\<version>` 保留版本并原子维护 `current.json`/`previous.json`。任务以当前用户 `InteractiveToken`、`LeastPrivilege` 注册登录触发。`/Run` 返回成功后还必须在默认 45 秒内看到目标 `/health/live.version` 和 writable readiness，否则自动停净失败版本、恢复指针并重启旧任务；首次安装失败会删除无效任务。任务层最多失败重启 3 次；`run-supervised.ps1` 还会持久化 10 分钟崩溃窗口，默认最多 5 次指数退避，随后写 `SUPERVISOR_CRASH_LOOP` 并停止。

`rollback.ps1` 在停止任务前分别校验当前/上一版配置，并要求二者解析到同一个规范化数据库路径；schema 检查始终固定读取当前活动数据库，即使借用另一版本中支持维护 CLI 的二进制，也不能误查目标配置指向的另一数据库。M3 数据库没有 M4 账本时按 `m4=0` 检查，不兼容时拒绝。成功路径只切换应用/配置指针，启动任务并检查 `/health/live` 的版本和 `/health/ready`；失败时先结束失败进程，再恢复原指针并重启原版本。Windows 结束计划任务不会自动保证其 `exam-monitor.exe` 子进程退出，因此安装、回滚和卸载共用 `process-control.ps1`：只识别 `%LOCALAPPDATA%\ExamMonitor\releases` 下路径已验证的受管二进制，停止并等待清零，拒绝按未验证 PID 误杀其他进程。`uninstall.ps1` 删除任务和受管进程，但版本、配置、状态和 Evidence 均保留。

M4 故障矩阵可运行 `.\scripts\fault-injection.ps1`；M5 完整真实闭环由 `.\scripts\smoke.ps1` 执行，除原有恢复/回滚场景外还从候选二进制打开嵌入页面、资源和汇总，所有破坏性场景使用临时目录和唯一临时任务名。回滚 smoke 从固定提交 `89ed656` 构建真实 M3 二进制，先安装 M3 再安装候选，并在同一前向数据库上启动回滚后的 M3；不是只改版本字符串重编候选。

M6 使用 `.\scripts\m6-certification.ps1` 密封候选、配置、依赖、来源 SLA、外部书桌媒体发布器、资源/RPO/RTO 和故障窗口。初始化复跑测试与冻结候选 smoke，并注册 5 分钟资源采样、每日真实书桌媒体、每日完整备份和次日覆盖率/完整性汇总任务。媒体发布器独立调用固定 FFmpeg，遵循 M2 三文件协议并等待 accepted 确认，不进入 Recorder Core；在线资源样本来自进程指标、`/api/v1/operations/status` 的 Go runtime 计数和受管目录大小；每日数据库计数只读取日界线后完成的完整备份一致快照，并校验实际备份间隔。完整命令、证据目录和重新计时规则见 [`M6_CERTIFICATION.md`](M6_CERTIFICATION.md)。

## 9. 升级

正式冻结后关闭自动更新。升级只能在非备考阶段或明确维护窗口进行，并且任何行为或配置变化都需要重新通过相应测试；M6 认证期间发生这类变化必须重新计时。
