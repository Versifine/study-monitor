# M2 媒体分段导入合同

本文冻结 Version 1 外部媒体分段的文件合同、验证边界、受管存储顺序、幂等语义和恢复行为。M2 只接收外部录像器已经完成的短视频分段；不实现摄像头或屏幕录像器、不分析视频内容、不登记任意外部路径，也不执行 M4 的保留删除或磁盘三级保护。

## 1. 启用条件和目录

媒体入口默认关闭。启用时必须配置：

- `media_ingest.enabled = true`
- 绝对 `media_ingest.ffprobe_path`，且 `ffprobe -version` 必须报告 `N-117599-ge1d1ba4cbc-20241017`
- 与受管目录不重叠的 `media_ingest.inbox_directory`；相对值固定在 `paths.data_directory` 内解析，不依赖进程工作目录

Recorder Core 固定使用以下目标目录，sidecar 不能选择目的路径：

```text
<data_directory>/media/
├─ staging/       # 同目标卷临时副本
├─ accepted/      # 已校验并完成原子改名的媒体
└─ quarantine/    # 损坏或不合规副本、sidecar 和原因
```

入口和所有受管路径在使用前都规范化。入口只接受根目录普通文件；路径穿越、子目录、符号链接、Windows junction 和其他 reparse point 均被拒绝。

## 2. 来源发布协议

一个来源分段由同一入口根目录下的三个文件组成：

```text
segment.mp4
segment.mp4.sidecar.json
segment.mp4.ready
```

来源必须按以下顺序发布：

1. 完成并关闭媒体文件，不再修改其内容、大小或修改时间。
2. 写完 sidecar，并把 `complete` 设为 `true`。
3. 最后原子发布空的 `.ready` 标记。
4. 保留以上来源文件，直到出现有效的 `segment.mp4.accepted.json`。
5. 来源采集器验证确认内容后，才可以按自己的策略清理来源；Recorder Core 不删除来源文件。

没有 `.ready` 的文件不会被扫描。存在 `.ready` 但媒体或 sidecar 缺失、sidecar 尚未完成、或媒体在 `settle_interval` 两次检查间仍在增长时，保持 pending，不进入 `accepted`。

## 3. Sidecar schema v1

sidecar 是 UTF-8 JSON 根对象。字段名必须与下列 schema 大小写完全一致；未知字段、大小写别名、重复 key、尾随 JSON 和缺失字段均被拒绝。

```json
{
  "schema_version": 1,
  "complete": true,
  "collector_id": "desk.camera",
  "source_idempotency_key": "desk-20260719-100000-0001",
  "device_start_raw": "2026-07-19T10:00:00+08:00",
  "device_end_raw": "2026-07-19T10:00:01+08:00",
  "clock_offset_ms": 0,
  "clock_error_ms": 25,
  "size_bytes": 2195,
  "sha256": "346f4da339e0f6f91e1785436c74f97a42b87392e6f4939b2fc01b5e12f442d8",
  "media_type": "video"
}
```

字段约束：

- `schema_version` 必须是 `1`，`complete` 必须是 `true`。
- `collector_id` 和 `source_idempotency_key` 分别不超过 128 和 256 字节，不允许空白或控制字符。
- 设备起止时间必须是带 `Z` 或明确 UTC offset 的 RFC3339；未知 offset `-00:00` 被拒绝。结束时间必须晚于开始时间，且分段不超过配置上限；Version 1 上限不能高于 10 分钟。
- `clock_offset_ms = server_utc - device_utc`，必须显式提供；`clock_error_ms` 必须显式提供且非负。
- `size_bytes` 必须为正且不超过配置上限；`sha256` 必须是 64 位小写十六进制。
- M2 的 `media_type` 只接受 `video`。

## 4. 验证和提交顺序

对于每个 ready 分段，Recorder Core：

1. 验证入口路径、sidecar schema、时间和配置上限。
2. 在 `settle_interval` 前后检查来源大小和修改时间，拒绝仍在增长的文件。
3. 跨来源卷读取媒体，同时复制到目标卷 `staging/*.partial`，限制总字节数并计算 SHA-256。
4. 比较实际大小和 sidecar；调用固定版本 ffprobe 验证存在可解码视频流、格式、编码和时长。
5. 对临时文件执行落盘同步，在目标卷内原子改名为 `accepted/<sha256>.media`，并再次同步。
6. 只有受管文件已经存在且校验通过后，才在一个 SQLite 事务中追加媒体身份、`accepted` 状态和导入事实。
7. 数据库提交成功后，向来源入口原子写入确认标记。

数据库绝不先于受管文件确认 `accepted`。若进程在目标改名后、数据库提交前退出，来源仍无确认；重启后同一 ready 分段会校验既有受管文件并收敛到同一媒体记录。若权威事实已提交而投影更新失败，事实不回滚；重试或启动时的投影重建完成收敛。

M2 的新增表由独立、只追加且带 SHA-256 校验的 `media_schema_migrations` 账本管理，不提高 M1 核心的 `PRAGMA user_version = 1`，也不向核心 `schema_migrations` 写入媒体版本。因而回滚到上一稳定 M1 应用时，它会安全忽略媒体账本和新增表，仍可启动并读取 `raw_events`；回滚不会执行 down migration 或删改媒体事实。

## 5. 幂等和冲突

来源身份是 `(collector_id, source_idempotency_key)`：

- 同一来源身份、相同 SHA-256 和相同验证元数据返回原 `media_segment_id`，不创建第二份媒体事实。
- 同一来源身份承载不同 SHA-256，返回 `MEDIA_IDEMPOTENCY_CONFLICT`；同一来源身份的内容相同但验证元数据改变，返回 `MEDIA_METADATA_CONFLICT`。两者都不覆盖原事实。
- 相同 SHA-256 只有在设备时间、时钟质量、大小、时长、编码、格式、类型和 sidecar 版本等元数据也一致时才复用原媒体；否则返回 `MEDIA_METADATA_CONFLICT`。

成功确认文件的 schema 为：

```json
{
  "schema_version": 1,
  "collector_id": "desk.camera",
  "source_idempotency_key": "desk-20260719-100000-0001",
  "media_segment_id": 1,
  "sha256": "346f4da339e0f6f91e1785436c74f97a42b87392e6f4939b2fc01b5e12f442d8",
  "metadata_hash": "1111111111111111111111111111111111111111111111111111111111111111",
  "accepted_at_utc": "2026-07-19T02:00:02Z"
}
```

来源应至少核对 schema、collector、来源幂等键、媒体 ID 和 SHA-256。确认缺失或损坏时，可以保留原三文件并安全重放。

## 6. 隔离和错误

损坏、伪造或不合规媒体不会进入 `accepted`。Recorder Core 在受管 `quarantine/` 中保存媒体副本、可用的原 sidecar 和 `*.reason.json`；来源文件保持不变。隔离本身失败时记录 `MEDIA_QUARANTINE_FAILED`，不伪造隔离或接受成功。

稳定错误码包括：

| 类别 | 错误码 |
|---|---|
| 路径 | `MEDIA_PATH_INVALID`、`MEDIA_REPARSE_POINT_REJECTED` |
| 未完成 | `MEDIA_SOURCE_MISSING`、`MEDIA_SIDECAR_MISSING`、`MEDIA_SIDECAR_INCOMPLETE`、`MEDIA_FILE_GROWING` |
| 完整性 | `MEDIA_TOO_LARGE`、`MEDIA_SIZE_MISMATCH`、`MEDIA_HASH_MISMATCH`、`MEDIA_TIME_INVALID` |
| 媒体探测 | `MEDIA_FFPROBE_UNAVAILABLE`、`MEDIA_FFPROBE_VERSION_MISMATCH`、`MEDIA_FFPROBE_FAILED`、`MEDIA_DURATION_INVALID`、`MEDIA_TYPE_INVALID` |
| 身份与存储 | `MEDIA_IDEMPOTENCY_CONFLICT`、`MEDIA_METADATA_CONFLICT`、`MEDIA_STORAGE_FAILED`、`MEDIA_DATABASE_FAILED`、`MEDIA_QUARANTINE_FAILED` |

ffprobe 缺失或版本不匹配只把媒体模块设为 unavailable。初始化后的扫描或数据库故障同样在媒体状态中显示 unavailable，但后台按配置的有界扫描间隔继续重试，下一次完整扫描成功后恢复 healthy。M1 事件写入、查询和 `/health/ready` 继续工作；媒体失败不能把核心事件存储伪装为不可用。

## 7. 状态和空间需求

`GET|HEAD /api/v1/media/ingest/status` 返回只读状态：

```json
{
  "schema_version": 1,
  "status": "healthy",
  "ffprobe_version": "N-117599-ge1d1ba4cbc-20241017",
  "last_scan_utc": "2026-07-19T02:00:03Z",
  "filesystem_ready_backlog": 1,
  "filesystem_ready_bytes": 2195,
  "ingest": {
    "backlog": 0,
    "discovered": 0,
    "pending": 0,
    "validated": 0,
    "quarantined": 1,
    "accepted": 1,
    "failed": 0,
    "total_segments": 1,
    "last_error_code": "MEDIA_HASH_MISMATCH"
  }
}
```

- `filesystem_ready_backlog` 是入口中没有有效 accepted 确认的 ready 文件数。
- `filesystem_ready_bytes` 是这些来源媒体当前普通文件大小的饱和求和，用于估算待处理空间；不可安全读取的对象按 0 字节计，但仍可计入数量。
- `ingest.backlog` 只统计数据库投影中当前为 discovered、pending 或 validated 的来源，不与文件系统数量重复相加。
- `status` 为 `disabled`、`healthy` 或 `unavailable`。该端点始终是状态查询；核心可写性仍以 `/health/ready` 为准。

M2 只计算和暴露上述需求，不根据磁盘水位删除、暂停或降级。预警/关键/保留空间水位和破坏性保留执行属于 M4。

## 8. 资源上限和验证

配置限制扫描间隔、稳定等待、单段字节数、单段时长、sidecar 字节数、每轮最多处理的 ready 条目和 ffprobe 超时。默认最多处理 2 GiB、10 分钟的分段，每轮最多 1000 个 ready 条目；部署时可以收紧，不能把时长放宽到超过 10 分钟。

`scripts/smoke.ps1` 使用 `testdata/media/valid.mp4.b64` 解码固定的 1 秒 H.264 MP4，验证导入、确认重放、校验和错误隔离、来源保留和重启恢复。Go 集成测试还使用同一夹具的真实截断副本（其 sidecar 大小与 SHA-256 与截断内容匹配），并由固定 ffprobe 证明它被隔离；另以子进程在复制中和改名后提交前真实退出，重新打开 SQLite 后验证无部分接受事实且可安全重放。测试只使用临时目录，不写生产数据。
