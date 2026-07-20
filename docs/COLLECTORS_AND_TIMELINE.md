# M3 采集器、覆盖率与统一时间线

本文冻结 M3 的 ActivityWatch 只读导入、通用 JSON Evidence、追加心跳、覆盖率投影和统一时间线合同。M3 只保存和投影 Evidence 事实，不创建 Observation、Inference、知识图谱、原生手机/健康连接器或分析队列。

## 1. 采集器配置

每个启用采集器必须配置稳定 `id`、`kind`、`heartbeat_period`、`allowed_lateness`、`offline_after` 和按 IANA 时区定义的每周 `planned_schedule`。`Local` 不属于冻结时区；时段采用本地半开区间，`24:00` 只可作为结束，重叠计划窗口在投影前合并。IANA 时区数据库通过 Go `time/tzdata` 固定嵌入最终二进制，生产机无需安装 Go 或另配 `ZONEINFO`。启用的 ActivityWatch 必须显式给出全部嵌套字段和 1..65535 的 loopback 端口，`clock_error_ms` 不得因缺省而伪装成零误差。

关系必须满足：

- `heartbeat_period + allowed_lateness <= offline_after`
- ActivityWatch `poll_interval <= heartbeat_period`
- ActivityWatch `offline_after <= 5m`
- 媒体 `offline_after <= 15m`
- M2 媒体分段仍不得超过 10 分钟

`runtime.mode` 只接受 `record-only` 或 `minimum`。`minimum` 必须保留备份接口配置，且只可启用 ActivityWatch 和媒体采集器；核心事件存储、历史只读查询与健康接口始终启用，但通用 Event/Evidence 和外部心跳三个 POST 入口返回 `API_MODULE_DISABLED`。ActivityWatch 与媒体管理器直接写入 Core，不经过这些外部入口。M3 不实现 M4 的实际备份、安装或自动恢复脚本。

## 2. ActivityWatch 只读适配

每个 ActivityWatch 配置绑定一个明确 bucket。适配器只访问 loopback 明文 HTTP origin，并且只调用：

- `GET /api/0/buckets/{bucket_id}`
- `GET /api/0/buckets/{bucket_id}/events?start=...&end=...&limit=...`

适配器不打开、写入、锁定或迁移 ActivityWatch 数据库，也不调用其 POST、PUT、PATCH 或 DELETE 接口。bucket 元数据和事件使用冻结的 JSON 字段白名单；单响应最多 8 MiB，单轮跨页累计最多估算 32 MiB/10000 个唯一事件，所有 ActivityWatch worker 共用 2 个 poll 槽，启用的 ActivityWatch 采集器最多 4 个。整轮执行预算按最短 `offline_after` 和实际 poll 波次计算；排队结束、取得槽位后才冻结本轮 `now` 和迟到截止点。每轮结束主动关闭该轮 transport 的空闲连接，请求时间、页数和每页事件数仍分别有上限。

导入截止点是 `now - allowed_lateness`；只有 `timestamp + duration` 不晚于截止点的完整事件才可导入，避免读取仍可能被 ActivityWatch heartbeat 合并修改的末尾事件。若较早 tuple 仍可变，即使更晚事件已闭合也只能返回该 tuple 之前的稳定前缀，断点不得越过 deferred 事实。已保存断点前保留 `rescan_window` 重叠重扫；来源身份由 bucket 哈希和 ActivityWatch event ID 组成。相同内容重扫返回 `duplicate`，不同内容返回冲突并停止推进断点。

事件按来源时间和来源 ID 升序写入。每个成功批次提交后才单调推进 Core 内的 `(source_time_utc, source_event_id)` 断点；进程在事实提交后、断点更新前终止只会导致安全重放。分页不前进、响应损坏、积压超过 `max_pages_per_poll` 或写入失败时不越过未确认事实。

一个 ActivityWatch bucket 失败只把该采集器状态标为 `unavailable`；其他采集器、M1/M2 写入、时间线和 Core readiness 继续工作。成功 GET 会追加一条表示适配器读路径存活的 `active` 心跳；ActivityWatch AFK bucket 中明确的 `status=afk` 事件才可投影为 `confirmed_idle`。

## 3. 通用 JSON Evidence

`POST /api/v1/evidence/batch` 是 M3 的通用入口，并与 M1 `POST /api/v1/events/batch` 使用完全相同的 envelope、逐项结果、幂等键、时间、大小、深度、Origin 和事务合同。旧路径继续保留；两个路径写入同一 `raw_events` 事实表。

手机和健康数据在 Version 1 只能由外部工具转换后走该入口。Recorder Core 不提供原生手机应用、健康平台 SDK 或远程公网接收服务。

## 4. 追加心跳

`POST /api/v1/collectors/heartbeats/batch` 接收精确区间事实：

```json
{
  "schema_version": 1,
  "heartbeats": [
    {
      "schema_version": 1,
      "collector_id": "desktop.activity",
      "state": "idle",
      "device_start_raw": "2026-07-20T10:00:00+08:00",
      "device_end_raw": "2026-07-20T10:01:00+08:00",
      "clock_offset_ms": 0,
      "clock_error_ms": 25,
      "idempotency_key": "stable-heartbeat-key",
      "quality_flags": []
    }
  ]
}
```

`state` 只接受 `active` 或 `idle`；区间必须为正且不超过该采集器的 `heartbeat_period`。设备时间必须带已知 RFC3339 offset，`-00:00` 被拒绝。质量标记只接受 `obscured`、`corrupt`、`clock_uncertain`、`incomplete`，拒绝重复值。

心跳按 `(collector_id, idempotency_key)` 幂等，只追加且由数据库触发器拒绝 UPDATE/DELETE。同键不同内容返回 `HEARTBEAT_IDEMPOTENCY_CONFLICT`，不覆盖旧事实。

## 5. 覆盖率投影

`GET /api/v1/coverage?start=...&end=...&collector_id=...` 按启用采集器重建请求范围，并事务性替换该采集器上一份缓存。计划启用时间 100% 落入一个且仅一个半开区间状态：

- `covered`
- `confirmed_idle`
- `pending`
- `delayed`
- `offline`
- `unknown`

质量标记独立于可用性并取覆盖事实的并集。重叠事实用固定优先级和稳定 ID 决定单值可用性，相邻且状态、原因、标记相同的区间合并。投影表强制正区间和唯一边界；测试同时检查相邻边界、重叠来源和重复重建。

只有显式 `idle` 心跳或 ActivityWatch AFK 事件能生成 `confirmed_idle`。无事实的近期尾部可为 `pending`；超过允许迟到后转为 `unknown`/`delayed`，超过 `offline_after` 后为 `offline`。迟到心跳或 ActivityWatch/媒体区间会在重建时覆盖相应缺口，但不会修改原始事实。

每个采集器单独重建。失败采集器的投影状态为 `stale` 并保留事实；其他采集器继续返回 `fresh`。查询范围和参与事实数有配置上限；单次同步另有固定 32 MiB 估算内存预算。SQLite 表达式/范围索引先产生有界候选流，再以 Go 纳秒时间做精确半开边界判断；只有真实命中才计入事实和字节预算，边界外突发不能挤掉范围内事实。覆盖率专用同步只计 ActivityWatch 区间、心跳和媒体区间，通用 point Evidence 仍保留在事实库且可由时间线投影，但不能耗尽覆盖率预算。时间线与覆盖率投影在 HTTP 层单并发，Store 内有界等待串行，并以最多 128 行的小事务写入；未变化投影不执行 UPDATE。超过事实/字节预算时安全失败，不阻断 Evidence 事实写入。

## 6. 统一时间线

`GET /api/v1/timeline?start=...&end=...&limit=...&cursor=...` 合并：

- `raw_event`
- `heartbeat`
- `media_segment`

每项返回稳定来源 ID、原始设备开始/结束时间、设备 UTC、服务端接收 UTC、校正开始/结束时间、`clock_offset_ms`、`clock_error_ms`、`clock_uncertain`、质量标记和原始/确定性元数据 payload。原始时区表示保留，因此夏令时回拨中相同本地钟面时间仍可区分。

合法的 ActivityWatch 零时长事件保留为点事实，不产生覆盖区间。通用 Evidence 若使用保留的 `activitywatch.event` 名称但 payload 不满足适配器冻结结构，则保留为带 `incomplete` 的点事实，不能让一个不可删除的坏事实堵塞时间线或其他采集器覆盖率。

校正时间使用 `device_utc + clock_offset_ms`，以固定 9 位纳秒 UTC 文本进入可重建投影。误差大于 `timeline.clock_uncertain_after` 时显式加入 `clock_uncertain`，不声称精确顺序。

排序键固定为校正开始 UTC、接收 UTC、来源类型、来源 ID、投影 ID。首次查询同步投影并冻结 `snapshot_id`；cursor 同时绑定查询范围和最后排序键，续页直接读取该快照，不再重跑全范围同步，分页期间新增事实不会进入该快照。

## 7. 固定测试和 smoke

ActivityWatch 固定夹具位于 `internal/collectors/testdata/`。测试覆盖 GET-only、真实多页边界去重、分页 stalled/backlog、跨页字节预算、全局 poll 并发、整轮超时、排队后时间冻结、每轮空闲连接关闭、不得越过较早 deferred 事件、断点跨重启、事实提交与断点提交间崩溃重放、重扫、损坏响应、单来源离线隔离、心跳重复/冲突、相接和重叠区间、迟到事实、时钟前后校正、DST 原始 offset、大误差、稳定分页、源查询 `EXPLAIN QUERY PLAN` 索引和精确边界预算。

`scripts/smoke.ps1` 使用临时目录和真实二进制，复验 M1/M2 后生成通用事件、追加心跳和已接受媒体三类时间线来源，再配置一个没有事实的采集器并确认其覆盖率出现 `offline` 而不是 `confirmed_idle`。smoke 不接触真实 Evidence 或 ActivityWatch 数据。
