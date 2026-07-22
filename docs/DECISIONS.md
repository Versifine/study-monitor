# 关键决策记录

## D001 主目标

以考研为主，完整生活记录是数据底座，不是第一产品目标。

## D002 主动测评

允许系统主动出题、追问和延迟复测。

## D003 纸笔介质

不迁移到平板或数位板，必须支持普通纸笔。

## D004 视觉路线

使用视觉语言模型直接理解纸面和过程，OCR 仅辅助。

## D005 云端与本地

允许使用云端模型，本地 RTX 4060 负责低成本检测和补充算力。

## D006 可穿戴目标

希望接近手表级、尽可能全天佩戴；接受最终采用轻量哨兵加手机/腰间计算模块。

## D007 正式备考期间开发

正式备考后不持续开发。普通误判、UI 和模型效果问题不得触发即时调试。

## D008 Version 1

先完成 Recorder Core；分析层只能在 Version 1 之后的独立里程碑中实验化、可关闭、影子运行。

## D009 Version 1 模式边界

Version 1 的标准验收模式是 record-only，紧急降级模式是 minimum。包含分析和教练能力的增强模式属于后续研究，不是 Version 1 的完成条件。

## D010 Version 1 数据源边界

Version 1 原生支持 ActivityWatch、外部媒体受管导入和通用 JSON Evidence。手机与健康数据只能使用通用合同导入，不开发原生手机应用或健康平台连接器。

## D011 媒体所有权

Version 1 只把已复制到 Recorder Core 管理目录并完成校验和数据库确认的文件视为 `accepted`。不把任意外部路径登记为稳定证据；来源文件默认由来源采集器在收到确认后清理。

## D012 仅追加与投影

Evidence、心跳、媒体导入/状态和故障事实只追加；采集器当前状态、媒体当前状态、覆盖率和健康摘要是可重建投影。投影更新失败不得删除或改写权威事实。

## D013 覆盖率模型

覆盖率按采集器分别计算。可用性使用单值状态，遮挡、损坏和时钟不确定性使用独立质量标记；没有有效心跳时不得把缺少数据解释为确认空闲。

## D014 部署冻结清单

采集周期、迟到/离线门槛、磁盘水位、保留条件、资源上限和 RPO/RTO 是按目标机器配置的部署参数。M4 提供安全默认值和校验，M6 开始前锁定并记录校验和；认证中不得修改或放宽。

## D015 回滚边界

回滚只切换到上一稳定应用和兼容配置，不执行破坏性数据库 down migration。每个候选版本必须保持与上一稳定版本的 schema 兼容窗口；不兼容时脚本安全拒绝，而不是冒险改写 Evidence。

## D016 Windows 进程托管

Version 1 使用当前用户的 Windows Task Scheduler 任务作为生产监督器：登录后启动、失败后有界重启、崩溃循环后停止并暴露故障。这样可与用户会话中的 ActivityWatch 和本地目录一致工作，且不要求管理员权限、WSL 或第三方服务包装器。M4 的安装/卸载脚本负责注册和移除任务，卸载不删除数据。

## D017 配置格式

Version 1 使用版本化 JSON 配置和环境变量覆盖，优先使用 Go 标准库解析；不同时支持 YAML/TOML 等多套格式。示例配置提交 Git，本机配置和运行数据不提交。

## D018 Version 1 保留对象

Version 1 的自动保留执行只允许处理受管媒体本体；结构化事件、心跳、故障、媒体元数据和媒体状态历史不自动删除。达到空间极限时先停止确认新写入，不通过清理审计事实继续运行。

## D019 进度与上线门槛

时间不是 Version 1 的首要风险。2026 年 9 月开始持续备考不等于 Recorder Core 必须上线；只有 M0-M6 全部通过后系统才成为正式依赖。未通过时继续使用现有工具，不压缩测试、不跳过恢复演练，也不并行扩大范围。

## D020 数据目录相对路径基准

Windows 生产默认应用数据根固定为 `%LOCALAPPDATA%\ExamMonitor`，`paths.data_directory` 的相对值只能在该根内解析，不依赖当前工作目录；如需其他卷，必须显式配置绝对路径。`scripts/dev.ps1` 将开发数据目录显式覆盖为仓库根下被 Git 忽略的 `data/`。配置检查只解析和校验路径，不创建目录。

## D021 M1 SQLite 驱动与离线构建

M1 固定使用 `modernc.org/sqlite v1.54.0`，不依赖 CGO。模块及传递依赖提交到 `vendor/`；Windows dev/test/build 脚本固定 `GOTOOLCHAIN=local`、`GOPROXY=off` 和 `-mod=vendor`，退出时恢复调用者环境。Recorder Core 运行和已冻结构建均不依赖网络服务。迁移 SQL 在 Git 中固定 LF，保证 Windows checkout 不改变嵌入字节和已记录 checksum。

## D022 M1 幂等内容定义

`(collector_id, idempotency_key)` 是唯一键。`payload_hash` 对解析后的 payload 规范化 JSON 计算 SHA-256；`content_hash` 对 schema、collector、event type、原始设备时间、设备 UTC、时钟偏移/误差和 `payload_hash` 计算 SHA-256，不包含服务端接收时间。同键同 `content_hash` 返回原事件 ID，同键不同内容返回冲突，均不覆盖原事实。

## D023 M1 批次提交边界

结构合法批次中的损坏项在进入数据库前得到逐项 `rejected`；其余合法项在一个 SQLite 事务中提交。只有事务成功提交后才返回 `accepted`、`duplicate` 或 `conflict`；存储错误时整个合法子集不确认，来源可用同一幂等键安全重试。

## D024 M1 查询快照

事件查询按单调 `raw_events.id` 升序。首个请求冻结当时最大 ID，后续不透明 cursor 同时携带该 snapshot ID 与上一条 ID；分页期间的新写入不进入已发出的快照。M1 不提供分析筛选、全文检索或可修改游标状态。

## D025 M2 文件合同与媒体探测

M2 使用入口根目录中的 `<media>`、`<media>.sidecar.json` 和最后发布的 `<media>.ready` 作为唯一来源合同；成功后只写 `<media>.accepted.json`，不删除来源。sidecar v1 使用大小写完全一致的固定字段白名单，并拒绝未知字段、大小写别名、重复 key、`-00:00` 和超过 10 分钟的分段。媒体内容由固定版本 `ffprobe N-117599-ge1d1ba4cbc-20241017` 验证；缺失或版本不符只停用媒体入口，不影响 M1 核心事件 API。

## D026 M2 受管提交与恢复边界

媒体目的路径只由 Recorder Core 在 `<data_directory>/media` 下生成。来源内容跨卷复制到目标卷临时文件，落盘、校验并同卷原子改名后，数据库才允许追加 `accepted`；随后才向来源写确认。改名后数据库失败通过原 ready 文件重试收敛，数据库不得指向不存在的 accepted 文件。媒体身份、导入和状态事实只追加，投影可重建；隔离保留原因且不删除未知或未确认来源。M2 只暴露入口 ready 数量和字节需求，不执行 M4 的磁盘水位或保留删除。

## D027 M2 迁移与上一稳定版兼容

M1 核心 schema 继续使用 `PRAGMA user_version = 1` 和 `schema_migrations`。M2 的纯新增媒体表使用独立、只追加且带嵌入 SQL SHA-256 校验的 `media_schema_migrations`，不提升核心版本、不修改旧表语义。上一稳定 M1 应用因此可以忽略新增表和媒体账本，继续启动并读取其已知数据；应用回滚不执行 down migration。后续里程碑若新增不影响上一稳定版的模块结构，应沿用独立兼容账本或在实施前另行冻结兼容方案。

## D028 M3 ActivityWatch 读取与断点

每个 ActivityWatch 采集器只绑定一个明确 bucket，只允许访问 loopback `GET` bucket 元数据和事件接口。Recorder Core 不打开或修改 ActivityWatch 数据库。导入排除允许迟到窗口并重扫重叠区间；来源 event ID 构成幂等身份，Core 侧断点只在事实事务成功后单调推进。分页不前进、响应损坏或有界页数不足时停止并保留旧断点，不静默越过积压。

## D029 M3 心跳与覆盖率语义

心跳是带明确设备开始/结束时间的追加区间事实，`active`/`idle` 只描述外部采集器显式上报。覆盖率按计划启用时段和采集器分别重建，使用冻结单值可用性与独立质量标记；只有 `idle` 心跳或 ActivityWatch AFK 事实可产生 `confirmed_idle`。迟到事实重建投影但不修改事实，每个采集器用独立事务隔离陈旧投影。

## D030 M3 时间线与兼容迁移

统一时间线合并 raw event、heartbeat 和 accepted media 的可重建投影，排序冻结为校正时间、接收时间、来源类型、来源 ID 和投影 ID，并使用绑定范围的快照 cursor。M3 新表使用独立、只追加且校验嵌入 SQL SHA-256 的 `m3_schema_migrations`；核心 `PRAGMA user_version` 保持 1，上一稳定 M2 可忽略 M3 表启动，不执行 down migration。

## D031 M4 磁盘门与清理边界

磁盘状态使用可注入的可用字节探针，关系固定为 `warning > critical > database reserve`。warning 起暂停媒体导入，critical 继续允许小型核心事件，reserve 时拒绝并不确认核心写入。日志按大小/份数轮转，WAL 有界 checkpoint；临时清理只认 Core staging 内严格命名、超龄、普通且未被最新导入事实引用的 `.partial`，不实现通用目录清理。

## D032 M4 保留删除证明

自动保留默认关闭。启用后只在磁盘压力下处理 Recorder Core `media/accepted` 中、数据库状态为 accepted/restored、超过最短年龄、大小/SHA-256 匹配且被已验证完整备份 manifest 覆盖的媒体本体。每轮先从 manifest 建元数据索引，再只校验有界删除候选对应的备份本体，不能因确认覆盖而重哈希整份备份。删除前追加 `planned`，删除后追加 `deleted` 和 `retention_deleted`；缺少预先 `planned` 的文件缺失不得自动归类为保留删除。

## D033 M4 备份恢复原子边界

数据库快照使用 SQLite `VACUUM INTO` 并做完整性/迁移账本校验。备份 manifest 最后写入，完整备份只有所有列出文件哈希通过后才原子发布并更新 last-good marker。恢复先验证整份备份，默认写新目录且不切换；已有目标必须显式确认并先可恢复地移走。备份/恢复中断不修改当前 Evidence，也不替换 last-good。

## D034 M4 发布与回滚兼容窗口

每个构建输出含二进制哈希、配置和 core/media/M3/M4 schema 范围的 release manifest。安装原子维护 current/previous 应用指针。回滚在停止任务前检查当前四账本是否落在上一版本范围内，只切换二进制和配置并做健康检查；不执行 down migration、不删除 Evidence、不恢复数据库快照。

## D035 M5 零依赖嵌入构建

M5 固定 Node.js 24.11.1，只使用内置 TypeScript 类型擦除，`dependencies` 与 `devDependencies` 均为空。源文件确定性生成并校验已提交的 `web/dist`，由 Go `embed` 进入候选二进制；Node.js 只属于源码构建工具，不是生产运行依赖。前端使用系统字体和原生 HTML/CSS/TypeScript，不引入框架、外部字体、CDN、开发服务器或独立服务。

## D036 M5 只读与缺失语义

仪表盘只注册 GET/HEAD 静态资源和有界汇总，时间线每页 50 条且单次最多 200 条，覆盖率单次最多渲染 200 个区间；三类查询串行且不自动轮询。`disabled`、`not_installed`、`offline`、`delayed`、`unknown`、无数据和已确认零必须分开显示；外部积压没有上报合同即返回 `null/unknown`。M5 不增加迁移、写 API、分析队列或持久化 UI 状态。

## D037 M5 可关闭与 M4 回滚兼容

仪表盘启用只通过不进入 JSON schema 的环境变量控制，Record-only 默认启用、Minimum 强制关闭。资源校验失败会记录模块不可用并不注册页面；健康、写入、采集、恢复与运维不依赖处理器。M5 不改变配置 schema 和四个数据库账本，因此 M4 的 release manifest/安装/回滚兼容合同保持不变。
