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
- `schema_version`

幂等唯一范围是 `(collector_id, idempotency_key)`。同键同 `payload_hash` 是成功重放；同键不同内容是冲突，不得覆盖。

### collector_heartbeats

追加式保存采集器版本、设备时间、接收时间、CPU、内存、磁盘、外部队列、采集状态和错误码。采集器当前状态由最新有效心跳投影得到。

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

### media_ingest_events

追加式保存媒体导入尝试：来源幂等键、来源文件指纹、`discovered`、`pending`、`validated`、`quarantined`、`accepted` 或 `failed`、临时文件引用、稳定错误码和发生时间。它用于中断恢复与积压/故障展示，不把尚未完成或损坏的来源文件伪装成已接受 Evidence。

### media_segment_state_events

与 `media_segments` 同一事务写入首个 `accepted` 状态，之后追加 `missing`、`restored`、`retention_deleted` 等状态及原因、生产者和时间。当前媒体状态由事件投影得到；非法状态转换必须拒绝。导入前的 `pending` 和 `quarantined` 属于 `media_ingest_events`。

### fault_events

追加式保存故障、严重级别、影响模块、首次/最近发生时间引用、恢复动作和结果。重复故障可以聚合显示，但原始事件不可被聚合行替代。

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

### health_summary

数据库、媒体存储、磁盘保护、模块状态和最近故障的只读摘要。它是展示缓存，不是权威历史。

## 3. Version 1 不创建的后续实体

以下概念仅用于未来扩展合同，不在 M1-M6 建表、迁移或提供业务 API：

- `observations`
- `inferences`
- `knowledge_nodes`
- `problem_attempts`
- 模型任务队列和提示词注册表

未来若进入独立里程碑，Observation 和 Inference 必须保存来源 Evidence 引用、生成时间、生产者名称与版本、输出 schema 版本、置信度、状态和替代解释。不得修改 Evidence。

## 4. 数据不变量

- `raw_events`、`collector_heartbeats`、媒体导入/身份/状态和故障事实只追加
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
- 所有版本和校验和写入构建/冻结清单
