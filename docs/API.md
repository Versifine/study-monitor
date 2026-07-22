# M1-M3 本地 API

本文冻结 M1 的仅追加事件写入/查询/健康接口、M2 的媒体入口状态，以及 M3 的通用 Evidence、心跳、采集器状态、覆盖率和统一时间线。所有接口默认只监听 loopback；媒体数据面使用受管文件入口而不是 HTTP 上传。M3 不包含前端、AI、原生手机/健康连接器或复杂认证。

## 1. 通用规则

- API JSON schema 版本为 `1`；请求 envelope 和事件根对象的字段名必须与本文 schema 大小写完全一致，未知字段、大小写别名、任何对象层级的重复 key、尾随 JSON 均被拒绝。`payload` 是采集器定义的 JSON 对象，其字段名保持大小写敏感且允许自定义，但仍受大小、深度和有效 JSON 检查约束。
- 写入端点只接受 `Content-Type: application/json`，任何非空 `Origin` 请求头都被拒绝，防止本地网页静默提交。
- 响应包含 `Cache-Control: no-store`；错误响应包含稳定错误码，不返回或记录 Evidence payload。
- 请求体、批量条数、单事件大小、payload 深度、并发写入和查询页大小均由 `api` 配置限制。

## 2. 写入事件

`POST /api/v1/events/batch`

```json
{
  "schema_version": 1,
  "events": [
    {
      "schema_version": 1,
      "collector_id": "desktop.activity",
      "event_type": "study.activity",
      "device_timestamp_raw": "2026-07-18T10:00:00+08:00",
      "clock_offset_ms": 125,
      "clock_error_ms": 50,
      "idempotency_key": "collector-stable-key-1",
      "payload": {
        "window": "notes"
      }
    }
  ]
}
```

`device_timestamp_raw` 必须是带 `Z` 或明确 UTC offset 的 RFC3339 时间；`-00:00` 表示 offset 未知，因此必须拒绝，`Z` 和 `+00:00` 均可接受。服务端原样保存该字符串，同时保存解析后的 `device_time_utc` 和服务端 `received_at_utc`。`clock_offset_ms = server_utc - device_utc`，`clock_error_ms` 必须非负。

结构合法批次按输入顺序返回逐项结果：

```json
{
  "schema_version": 1,
  "results": [
    {
      "index": 0,
      "status": "accepted",
      "event_id": 1
    }
  ]
}
```

- `accepted`：新事实已提交。
- `duplicate`：同一 `(collector_id, idempotency_key)` 和相同不可变内容已存在，返回原 `event_id`。
- `conflict`：同一唯一键对应不同内容，返回原 `event_id` 和 `EVENT_IDEMPOTENCY_CONFLICT`，不覆盖原事实。
- `rejected`：该项 schema、时间、大小或 payload 不合法；同批其他合法项仍可提交。

合法项在一个事务中提交。发生数据库锁、只读、取消或其他存储错误时，不返回逐项确认；调用方应保留原事件并使用相同幂等键重试。

## 3. 查询事件

`GET /api/v1/events?limit=100&cursor=...`

响应按 `id` 升序：

```json
{
  "schema_version": 1,
  "snapshot_id": 120,
  "events": [],
  "next_cursor": "opaque-base64url-cursor"
}
```

第一次查询把当时最大事件 ID 固定为 `snapshot_id`。`next_cursor` 是不透明值；继续使用它时，之后新增的事件不会进入该分页快照。没有下一页时省略 `next_cursor`。M1 只接受 `limit` 和 `cursor`，不提供分析筛选或修改接口。

每个返回事件包含原始设备时间、设备 UTC、接收 UTC、时钟偏移/误差、collector、payload、payload SHA-256 和 schema 版本。数据库内部 `content_hash` 用于幂等比较，不通过查询 API 暴露。

## 4. 健康接口

- `GET|HEAD /health/live`：只表示进程和 HTTP 服务存活，不声称存储可用。
- `GET|HEAD /health/ready`：主动探测数据库可写性和 schema；只有 `writable` 返回 HTTP 200。

非就绪状态返回 HTTP 503，并区分：

- `busy`：数据库在有界 busy timeout 内仍被占用。
- `read_only`：数据库处于只读降级，不能确认新 Evidence。
- `migration_failed`：迁移失败、checksum 不匹配或数据库版本比程序更新。
- `unavailable`：其他存储初始化或访问故障。

存储初始化失败时 `/health/live` 仍可用，但写入和查询返回存储不可用；系统不会伪造成功确认。

M4 数据库保留空间受威胁时，即使 SQLite 仍可探测为可写，`/health/ready` 也返回 HTTP 503、`status=unavailable` 和 `STORAGE_DATABASE_RESERVE_THREATENED`。事件/心跳写入在读取请求体前以 HTTP 507 拒绝，表示该批次未确认，可以在空间恢复后按幂等键重试。预警和关键水位不会阻断小型核心事件；它们先暂停/拒绝媒体。

## 5. 媒体入口状态

`GET|HEAD /api/v1/media/ingest/status`

该只读端点返回媒体模块的 `disabled`、`healthy` 或 `unavailable` 状态、固定 ffprobe 版本、最后扫描时间、入口未确认 ready 数量和字节估算，以及从仅追加事实重建的导入状态摘要。完整响应 schema 和字段语义见 [`MEDIA_INGEST.md`](MEDIA_INGEST.md)。

媒体模块缺失工具、版本不匹配或普通导入失败时，该端点仍返回 HTTP 200 和可见状态；这些故障不改变 M1 事件存储的 `/health/ready`。客户端不能把状态查询的 HTTP 200 解释为媒体健康，必须检查响应中的 `status`。

只接受 GET 和 HEAD；其他方法返回 405。

## 6. M3 通用 Evidence 与心跳

`POST /api/v1/evidence/batch` 是 `/api/v1/events/batch` 的等价通用入口，复用第 2 节的完整 schema 和幂等/事务合同。两个路径写入同一事实表；手机和健康数据在 Version 1 只能由外部工具转换后走该合同。`runtime.mode=minimum` 时两个通用写入口均以 `API_MODULE_DISABLED` 关闭。

`POST /api/v1/collectors/heartbeats/batch` 接受版本化 `heartbeats` 数组。每项保存稳定采集器、`active`/`idle`、原始设备开始/结束时间、设备 UTC、接收 UTC、校正时间、时钟偏移/误差、幂等键和冻结质量标记。区间不得超过配置心跳周期；未启用采集器、未知 offset、重复/未知字段和非法状态逐项拒绝。同键同内容返回原 `heartbeat_id`；同键不同内容返回冲突，事实不可修改或删除。`minimum` 模式下此外部入口同样关闭，内置 ActivityWatch/媒体路径不受影响。

完整请求示例、错误边界和配置关系见 [`COLLECTORS_AND_TIMELINE.md`](COLLECTORS_AND_TIMELINE.md)。

## 7. 采集器状态

`GET|HEAD /api/v1/collectors/status`

返回每个启用 ActivityWatch 适配器的 `healthy` 或 `unavailable`、稳定错误码、最近尝试/成功时间、断点和本进程导入/重复计数。状态接口 HTTP 200 不等于所有采集器健康；单个采集器失败不改变 `/health/ready`。

## 8. 统一时间线

`GET|HEAD /api/v1/timeline?start=<RFC3339>&end=<RFC3339>&limit=100&cursor=...`

`start`/`end` 必须带已知 offset，范围不得超过 `timeline.max_query_range`。响应合并 `raw_event`、`heartbeat`、`media_segment`，返回 `stable_id`、原始设备时间、设备 UTC、接收 UTC、校正时间、时钟偏移/误差、`clock_uncertain`、质量标记和来源 payload。

排序键是校正开始 UTC、接收 UTC、来源类型、来源 ID、投影 ID。首次响应同步投影并冻结 `snapshot_id`；不透明 cursor 绑定原查询范围和最后排序键，续页不再重建投影，后续新增事实不进入该分页快照。cursor 与不同范围混用必须拒绝。首屏/覆盖率投影共享单并发门，竞争请求返回 429；单次同步受 32 MiB 固定估算内存预算和 128 行写事务限制，超出事实或字节预算显式失败而不回滚 Evidence。

## 9. 覆盖率

`GET|HEAD /api/v1/coverage?start=<RFC3339>&end=<RFC3339>&collector_id=...`

`collector_id` 可省略以重建所有启用采集器。每个计划启用时间返回不重叠的 `[start_utc,end_utc)`，可用性只能是 `covered`、`confirmed_idle`、`pending`、`delayed`、`offline`、`unknown`；质量标记是独立数组。只有明确 idle 心跳或 ActivityWatch AFK 事实可产生 `confirmed_idle`。

`projections` 为每个采集器返回 `fresh`/`stale`、generation、事实水位和错误码。单个采集器重建失败不会阻止其他采集器；失败不删除事实。覆盖率预算只统计可形成区间的 ActivityWatch、心跳和媒体事实；通用 point Evidence 保留且仍可在统一时间线查询，但不消耗覆盖率事实预算。

## 10. M4 运维状态

`GET|HEAD /api/v1/operations/status`

返回 `schema_version=1`、`disk_level`（`normal|warning|critical|reserve`；存储初始化失败时为 `unavailable`）、`free_bytes`、最近 `checked_at_utc`、可选稳定 `error_code` 和 `retention`（默认 `disabled`）。该端点是只读状态，不提供改变水位、启用删除、执行备份或回滚的 HTTP 操作；高风险运维只能走本机 PowerShell 脚本。存储打开失败时不会以 `normal/free_bytes=0` 假报健康。

## 11. 默认限制

| 配置 | 默认值 |
|---|---:|
| `api.max_request_bytes` | 1 MiB |
| `api.max_batch_events` | 100 |
| `api.max_event_bytes` | 64 KiB |
| `api.max_payload_depth` | 16 |
| `api.max_concurrent_writes` | 4 |
| `api.default_page_size` | 100 |
| `api.max_page_size` | 500 |
| `storage.busy_timeout` | 5 秒 |
| `storage.max_open_connections` | 8 |
| `storage.warning_free_bytes` | 10 GiB |
| `storage.critical_free_bytes` | 5 GiB |
| `storage.database_reserve_bytes` | 1 GiB |
| `operations.wal_max_bytes` | 64 MiB |
| `logging.max_file_bytes` / `max_files` | 10 MiB / 5 |
| `retention.enabled` / `minimum_age` | `false` / 168 小时 |
| `media_ingest.max_segment_bytes` | 2 GiB |
| `media_ingest.max_segment_duration` | 10 分钟 |
| `media_ingest.max_sidecar_bytes` | 64 KiB |
| `media_ingest.max_scan_entries` | 1000 |
| `timeline.clock_uncertain_after` | 1 秒 |
| `timeline.max_query_range` | 744 小时 |
| `timeline.max_projection_facts` | 100000 |

生产数据库路径是 `<data_directory>/exam-monitor.db`。`--check-config` 只输出解析后的路径和配置摘要，不创建目录或数据库。
