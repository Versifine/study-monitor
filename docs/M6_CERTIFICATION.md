# M6 十四天稳定性认证执行手册

## 1. 当前状态

M6 认证工具已实现，但 14 天计时只有在冻结候选已安装、ActivityWatch 与书桌媒体两个核心来源真实可用、预检全部通过并生成密封清单后才开始。临时 smoke、单元测试和浏览器截图不能代替真实 14 天。

认证期间禁止增加功能、升级依赖、修改二进制、核心配置、schema、保留行为或采集 SLA。上述任一项变化都由工具记录为 `restart_required=true`，必须更换认证目录并从第 1 天重新开始。只修改认证结束后的报告文字且不改变工具、运行行为或统计口径，可以不重新计时，但必须在最终报告记录。

## 2. 冻结输入

[`configs/m6-certification.example.json`](../configs/m6-certification.example.json) 是部署参数模板。开始前复制为 Git 忽略的 `configs/m6-certification.local.json`，只允许按目标机器收紧或落定以下值：

- 5 分钟资源采样周期和 24 小时完整备份 RPO
- CPU、工作集、私有内存、句柄、线程、Go 协程/堆、WAL、日志与 staging 上限
- 核心进程、系统重启、ActivityWatch、媒体、备份恢复和回滚 RTO
- 第 2 至 12 天的预声明故障注入窗口

生产配置必须同时满足：

- `runtime.mode=record-only`、备份接口启用、自动保留关闭
- 至少一个非空计划窗口的 ActivityWatch 采集器和一个书桌媒体采集器
- ActivityWatch 只访问 loopback HTTP 且配置 bucket 当场可读
- 每个 ActivityWatch 采集器已有成功轮询；媒体入口至少真实接受一个预检分段、无未确认 ready 积压，并使用固定 `ffprobe N-117599-ge1d1ba4cbc-20241017`
- 当前安装指向候选发布目录，上一稳定发布目录完整且版本不同
- 候选 commit 与干净工作树 HEAD 一致，二进制、发布 manifest 和配置校验和一致

## 3. 初始化与自动任务

先构建并安装候选，再执行一次初始化。初始化会复跑完整测试与冻结候选 smoke、创建完整备份并恢复到新目录、生成密封清单，然后注册当前用户的 5 分钟采样、每日 00:10 完整备份和次日 01:00 汇总任务。日报只读取日界线后的已完成备份；若备份任务缺失会立即补做，但实际备份间隔超过冻结 RPO 加 5 分钟任务调度容差时仍判失败。

```powershell
$cert = 'D:\ExamMonitorCertification\m6-2026-07'
$app = Join-Path $env:LOCALAPPDATA 'ExamMonitor'
$current = Get-Content -Raw -Encoding UTF8 (Join-Path $app 'current.json') | ConvertFrom-Json
$previous = Get-Content -Raw -Encoding UTF8 (Join-Path $app 'previous.json') | ConvertFrom-Json

.\scripts\m6-certification.ps1 `
  -Action Initialize `
  -CertificationDirectory $cert `
  -BinaryPath (Join-Path $current.release_directory 'exam-monitor.exe') `
  -ConfigPath $current.config_path `
  -ReleaseManifestPath (Join-Path $current.release_directory 'release-manifest.json') `
  -PreviousReleaseDirectory $previous.release_directory `
  -BackupDirectory 'D:\ExamMonitorBackups' `
  -ProfilePath (Resolve-Path '.\configs\m6-certification.local.json')
```

初始化日不计入认证。清单把开始时间固定为下一个 Asia/Shanghai 自然日 00:00，结束时间固定为 14 个完整自然日之后。运行数据只写入独立认证目录和备份目录；不进入源码、生产 Evidence 目录或 Git。

任务可以非破坏性重装；认证完成后只删除三个认证任务，不删除报告、备份或 Evidence：

```powershell
.\scripts\m6-certification.ps1 -Action InstallTasks -CertificationDirectory $cert
.\scripts\m6-certification.ps1 -Action RemoveTasks -CertificationDirectory $cert
```

## 4. 证据结构

```text
<certification>/
├─ freeze-manifest.json          # 候选、配置、依赖、来源 SLA、资源/RPO/RTO 和故障窗口
├─ freeze-manifest.sha256        # 清单密封校验和
├─ tool/                         # 冻结后的认证脚本副本；计划任务只执行此副本
├─ preflight/                    # 测试、故障注入、基线完整备份/恢复和数据库快照
├─ samples/YYYY-MM-DD.jsonl      # 5 分钟资源、API、进程、任务与文件状态
├─ backups/*.json                # 每次计划完整备份的时间与 manifest 索引
├─ daily/YYYY-MM-DD.json         # 每日完整备份、完整性、覆盖率、媒体抽样和门槛结论
├─ records/events.jsonl          # 故障、恢复、回滚、人工干预和通知记录
├─ violations.jsonl              # 任何冻结输入变化或日报硬门槛失败；存在即不得通过
└─ final-report.json             # 满 14 天且全部门槛通过后才生成
```

数据库认证快照来自完整备份中的一致 SQLite 快照，不以在线文件大小代替完整性检查。它累计记录事件、心跳、媒体、故障、模式、保留和投影数量，并扫描绕过稳定幂等身份的潜在重复组。每日报告保存与上一日或起始基线的数量差值。

## 5. 覆盖率与故障窗口

每日覆盖报告直接查询冻结候选的 `/api/v1/coverage`：

- 每个核心来源的非空计划时间必须 100% 分类
- 排除预声明窗口后，ActivityWatch 与书桌媒体可用覆盖都必须至少 99%
- ActivityWatch 单次非计划 `offline + unknown` 不得超过 300 秒
- 书桌媒体单次非计划无有效媒体不得超过 900 秒
- `corrupt`、`incomplete` 不计可用；书桌媒体的 `obscured` 也不计可用
- 故障窗口只从覆盖率分母中单独排除；它仍必须有 `passed` 演练记录并满足冻结 RTO

演练完成后写入一条有限详情记录；不得把普通失败补写成计划演练：

```powershell
.\scripts\m6-certification.ps1 -Action Record -CertificationDirectory $cert `
  -RecordKind process_termination -RecordStatus passed `
  -RecordDurationSeconds 8.4 `
  -RecordDetail 'planned window day03; recovered in 8.4s; RTO 90s'
```

工具只接受在密封窗口内发生的演练结果，并把 profile 中的 `rto_key`、冻结 RTO 与实测秒数写入记录。声称 `passed` 却没有实测时长或超过 RTO 时会落为 `failed` 并要求从第 1 天重新开始。通知记录只接受 `-RecordSeverity P0` 或 `P1`；任何计划外人工干预记录也会使本次认证失败。

系统重启属于影响用户会话的操作，只能在清单预声明窗口且确认当前工作已经保存后执行。所有破坏性媒体、磁盘、写中断和坏备份场景继续使用临时目录或注入探针，禁止对生产 Evidence 目录执行。

## 6. 最终判定

最终器不会自动补日报、排除失败日、放宽阈值或缩短时间：

```powershell
.\scripts\m6-certification.ps1 -Action Finalize -CertificationDirectory $cert
```

只有同时满足以下条件才生成 `verdict=pass` 的 `final-report.json`：

- 当前时间达到冻结结束时间，14 份自然日日报全部存在且为 `pass`
- 密封清单、候选二进制、发布 manifest、配置和认证工具未变化
- 没有 `violations.jsonl`，没有计划外人工干预
- 11 类必需故障/恢复/备份/回滚演练均有 `passed` 记录
- 每日数据库完整性、潜在重复、媒体抽样、覆盖率、资源和采样连续性门槛全部通过

通过后停止开发并移除认证任务，不自动进入视觉理解、主动测评或其他研究里程碑。
