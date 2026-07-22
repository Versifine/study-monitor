# 数据模型

## 1. Version 1 权威事实

### collectors

采集器稳定身份和配置引用。它不是活动事实日志；“最后心跳”和“当前状态”属于可重建投影。

### raw_events

仅追加结构化 Evidence 事件。

必须字段：

- `id`
- `collector_id`
- `event_type`
- `device_timestamp_raw`
- `device_time_utc`
- `received_at_utc`
- `clock_offset_ms`
- `clock_error_ms`
- `idempotency_key`
- `payload_json`
- `payload_hash`
- `content_hash`
- `schema_version`

幂等唯一范围是 `(collector_id, idempotency_key)`。`payload_hash` 是 payload 的规范化 SHA-256；`content_hash` 覆盖全部不可变事件字段和 `payload_hash`，但不包含服务端接收时间。同键同 `content_hash` 是成功重放并返回原事件 ID；同键不同内容是冲突，不得覆盖。

### collector_heartbeats

M3 追加式保存采集器明确上报的时间区间：稳定采集器与幂等键、`active`/`idle`、原始设备开始/结束时间、设备 UTC、接收 UTC、校正开始/结束时间、时钟偏移/误差、冻结质量标记、内容哈希和 schema 版本。区间不得超过配置心跳周期。

`(collector_id, idempotency_key)` 唯一；同键同内容返回原心跳 ID，同键不同内容可见冲突。数据库触发器拒绝 UPDATE/DELETE。CPU、内存、磁盘、外部队列和版本等 M4 健康字段不能借 M3 提前塞入该冻结 schema；后续如需要必须使用独立版本化事实。

### media_segments

追加式保存媒体稳定身份和经验证元数据：

- `id`
- `collector_id`
- `source_idempotency_key`
- Recorder Core 管理的相对路径
- 原始设备开始/结束时间、归一化 UTC 时间和接收 UTC 时间
- 时钟偏移和误差
- 大小、时长、编码和媒体类型
- 校验和与 sidecar schema 版本
- 创建时间

已接受记录不因文件保留操作而删除或改写。`media_segments` 只在受管文件已经落盘并校验成功后插入；发现一个来源文件不等于创建已接受媒体。

M2 的唯一来源身份为 `(collector_id, source_idempotency_key)`；`sha256` 支持相同内容查重，`metadata_hash` 覆盖设备原始/UTC 时间、时钟质量、大小、ffprobe 时长/编码/格式、媒体类型、校验和和 sidecar 版本。受管相对路径固定指向 `media/accepted/<sha256>.media`，不保存或长期引用入口绝对路径。完整文件合同见 [`MEDIA_INGEST.md`](MEDIA_INGEST.md)。

### media_ingest_events

追加式保存媒体导入尝试：来源幂等键、来源文件指纹、`discovered`、`pending`、`validated`、`quarantined`、`accepted` 或 `failed`、临时文件引用、稳定错误码和发生时间。它用于中断恢复与积压/故障展示，不把尚未完成或损坏的来源文件伪装成已接受 Evidence。

M2 使用稳定 `event_key` 抑制同一阶段重试产生的重复事实；投影按同一 `ingest_key` 的最大事件 ID 重建当前状态。入口中无有效确认的 `.ready` 文件数量和字节估算来自文件系统扫描，不与数据库 pending 投影重复相加。

### media_segment_state_events

与 `media_segments` 同一事务写入首个 `accepted` 状态，之后追加 `missing`、`restored`、`retention_deleted` 等状态及原因、生产者和时间。当前媒体状态由事件投影得到；非法状态转换必须拒绝。导入前的 `pending` 和 `quarantined` 属于 `media_ingest_events`。

### fault_events

M4 追加式保存稳定 `event_key`、影响模块、P0-P3、`active/recovered/degraded/disabled`、稳定错误码、最多 1024 字符的非 Evidence 详情和 UTC 时间。重复故障可以聚合显示，但原始事件不可被聚合行替代。

### module_state_events

追加式保存可选模块的 `healthy/degraded/disabled/unavailable`、原因码和 UTC 时间。M4 启动时会明确记录保留执行器是 `RETENTION_ENABLED` 还是 `RETENTION_DISABLED`；状态变化不覆盖历史。

### mode_transition_events

追加式保存 `old_mode`、`new_mode`、操作人、触发方式、原因码和 UTC 时间。首次 M4 启动从 `unknown` 进入配置模式；相同模式重启不重复追加，配置切换会留下可审计事实，不允许 UPDATE/DELETE。

### retention_events

仅引用已存在 `media_segments`，追加 `planned/deleted/failed`、原因、受管相对路径和 UTC 时间。保留执行器先写 `planned`，只删除 `media/accepted` 内满足年龄与完整备份覆盖的普通文件；文件删除后，`deleted`、`media_segment_state_events.retention_deleted` 及当前状态投影在同一事务提交。事务失败时保留最新 `planned` 供下次重试；已有 `planned` 是把缺失文件收敛为已删除的必要证据。

## 2. 可重建投影

### collector_status

采集器最后心跳、当前运行状态、版本、时钟质量和外部积压。允许更新，必须能从 `collectors` 和 `collector_heartbeats` 重建。

### media_segment_status

每个已接受媒体的当前状态。允许更新，必须能从媒体状态事件重建。媒体入口当前积压另由 `media_ingest_events` 重建。

### coverage_intervals

按 `collector_id` 和 `[start, end)` 保存计算结果：

- `availability`：`covered`、`confirmed_idle`、`pending`、`delayed`、`offline`、`unknown` 之一
- `quality_flags`：可包含 `obscured`、`corrupt`、`clock_uncertain`、`incomplete`
- `calculation_version`
- `calculated_at`
- 依赖的最后事实位置或水位

同一采集器的已结算区间不得重叠；相邻且状态、标记和计算版本相同的区间可以合并。迟到事实允许重建投影，不修改原始 Evidence。

M3 的 `coverage_projection_state` 保存每个采集器最近请求范围、generation、`fresh`/`stale`、错误码、事实水位和构建时间。每个采集器在独立事务中替换自己的投影缓存。

### activitywatch_checkpoints

ActivityWatch 断点是可更新的导入投影，不是 Evidence。每个采集器保存绑定 bucket、固定格式 UTC 来源时间、来源 event ID 和更新时间；只允许按 `(source_time_utc, source_event_id)` 单调推进。事实成功但断点尚未推进时，重启会重扫并由 `raw_events` 幂等合同去重。

ActivityWatch 基础事实以 bucket 哈希和来源 event ID 作为幂等身份。来源若在基础事实写入后只单调延长 duration，不覆盖旧事实，而是以 duration 位模式派生的 revision 幂等键追加同一来源 ID 的新快照；非单调时长或其他内容漂移不得伪装成 revision。

### timeline_entries

`raw_events`、`collector_heartbeats` 和 `media_segments` 的可重建统一时间线投影。每个来源事实只对应一个 `(source_type, source_id)`，保存稳定 ID、原始/UTC/接收/校正时间、时钟质量、质量标记和来源 payload。固定 9 位纳秒 UTC 文本用于稳定排序；原始设备时间仍以事实中的原字符串返回。M3 为三类来源建立校正开始/结束范围索引；SQLite 只做候选预筛，最终纳秒级半开边界在进入投影预算前精确判断。

### health_summary

数据库、媒体存储、磁盘保护、模块状态和最近故障的只读摘要。它是展示缓存，不是权威历史。

M5 没有为仪表盘新增表、迁移或持久化缓存。`QueryDashboardHistory` 直接从 M4 只追加故障、模块状态和模式切换事实读取有界窗口；采集器、媒体与磁盘当前状态来自既有内存/可重建投影。外部积压未提供合同即返回 `null/unknown`，不能写入或推断为零。故障详情不进入浏览器汇总。

## 3. Version 1 不创建的后续实体

以下概念仅用于未来扩展合同，不在 M1-M6 建表、迁移或提供业务 API：

- `observations`
- `inferences`
- `knowledge_nodes`
- `problem_attempts`
- 模型任务队列和提示词注册表

未来若进入独立里程碑，Observation 和 Inference 必须保存来源 Evidence 引用、生成时间、生产者名称与版本、输出 schema 版本、置信度、状态和替代解释。不得修改 Evidence。

## 4. 数据不变量

- `raw_events`、`collector_heartbeats`、媒体导入/身份/状态和故障事实只追加；ActivityWatch 断点、时间线和覆盖率明确是可重建投影
- 每类追加事实都必须有生产者作用域内的幂等键；状态转换和故障重试不得生成重复事实
- 同一幂等键重复提交不得产生重复事实
- 相同幂等键承载不同内容必须可见地失败
- 媒体必须先在目标卷写临时文件，校验后原子改名，再在数据库确认接受
- 文件系统成功但数据库提交失败时必须可恢复；数据库不得指向未落盘的 `accepted` 文件
- 投影更新失败不得回滚或删除已经提交的权威事实
- 投影重建使用计算版本和事实水位，重复执行得到相同结果，并以事务方式替换完整区间
- 删除原始媒体必须经过明确保留策略；必须保留元数据、校验和和删除状态
- Version 1 不自动删除结构化事件、心跳、故障或状态历史；空间不足时按磁盘保护停止确认新写入
- 迁移不得破坏旧证据；上一稳定版本必须能读取或安全忽略新增结构

## 5. 时间原则

- 归一化时间字段统一存 UTC；UI 根据配置的用户时区显示
- 原样保存设备提供的时间表示，同时保存解析后的 UTC 和服务端接收 UTC；缺少时区/偏移且无法无歧义解析的输入必须拒绝
- `clock_offset_ms = server_utc - device_utc`，因此校正时间为 `device_time_utc + clock_offset_ms`；它是派生值，必须同时返回偏移估计和非负误差
- 时间线稳定排序键至少包含校正时间、接收时间、来源类型和稳定 ID
- 时钟误差超过来源配置阈值时添加 `clock_uncertain`，不得伪造精确顺序

## 6. 版本化

- 事件、sidecar、配置和 API schema 版本化
- 采集器和生产者版本化
- 覆盖率计算版本化
- Version 1 迁移默认只做向后兼容的新增；不得 drop/rename 旧表列或改变旧字段语义。数据库迁移只前向执行，应用回滚至少保持上一稳定版本可启动和读取其已知数据
- 核心事件 schema 使用 `PRAGMA user_version` 与 `schema_migrations`；M2 媒体新增结构使用独立的 `media_schema_migrations`；M3 心跳、断点和投影使用独立的 `m3_schema_migrations`；M4 故障/模块/保留事实使用独立的 `m4_schema_migrations`。四个账本都只追加并校验嵌入 SQL 的 SHA-256，M2-M4 都不提高核心版本号
- M4 数据库保持核心版本 `1`；兼容的上一稳定应用只读取它已知的账本与表，并安全忽略新增 M4 表。每个发布 manifest 明确记录 core/media/M3/M4 最小和最大兼容版本；回滚先检查再切换
- 所有版本和校验和写入构建/冻结清单
