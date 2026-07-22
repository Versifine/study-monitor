import { apiErrorCode, formatBacklog, formatBytes, presentState, qualityLabel, type StatePresentation } from "./state.js";

interface OperationsStatus {
  disk_level: string;
  free_bytes: number;
  error_code?: string;
  checked_at_utc?: string;
  retention: string;
}

interface MediaStatus {
  status: string;
  error_code?: string;
  last_scan_utc?: string;
  filesystem_ready_backlog: number;
  filesystem_ready_bytes: number;
  ingest: { backlog: number; pending: number; quarantined: number; failed: number; last_error_code?: string };
}

interface CollectorStatus {
  collector_id: string;
  kind: string;
  status: string;
  error_code?: string;
  last_attempt_utc?: string;
  last_success_utc?: string;
  checkpoint_utc?: string;
}

interface DashboardFault {
  module: string;
  severity: string;
  status: string;
  error_code: string;
  occurred_at_utc: string;
}

interface DashboardModuleState {
  module: string;
  status: string;
  reason_code: string;
  occurred_at_utc: string;
}

interface DashboardSummary {
  schema_version: number;
  generated_at_utc: string;
  runtime_mode: string;
  analysis: { status: "not_installed"; backlog: null; last_updated_utc: null };
  operations: OperationsStatus;
  media: MediaStatus;
  collectors: CollectorStatus[];
  external_backlogs: Array<{ collector_id: string; status: "unknown"; items: null; bytes: null; last_updated_utc: null }>;
  history: {
    recent_faults: DashboardFault[];
    modules: DashboardModuleState[];
    mode?: { old_mode: string; new_mode: string; reason_code: string; occurred_at_utc: string };
  };
}

interface TimelineEntry {
  projection_id: number;
  stable_id: string;
  source_type: string;
  collector_id: string;
  event_type: string;
  device_start_raw: string;
  corrected_start_utc: string;
  corrected_end_utc?: string;
  received_at_utc: string;
  clock_error_ms: number;
  clock_uncertain: boolean;
  quality_flags: string[];
}

interface TimelinePage {
  entries: TimelineEntry[];
  next_cursor?: string;
}

interface CoverageInterval {
  projection_id: number;
  collector_id: string;
  start_utc: string;
  end_utc: string;
  availability: string;
  quality_flags: string[];
  reason_code: string;
}

interface CoverageResponse {
  projections: Array<{ collector_id: string; status: string; error_code?: string; built_at_utc: string }>;
  intervals: CoverageInterval[];
}

const timelineLimit = 50;
const timelineMaximum = 200;
const coverageMaximum = 200;
let timelineCursor = "";
let timelineEntries: TimelineEntry[] = [];
let dayStart = "";
let dayEnd = "";

function byID<T extends HTMLElement>(id: string): T {
  const value = document.getElementById(id);
  if (!(value instanceof HTMLElement)) throw new Error(`DASHBOARD_ELEMENT_MISSING:${id}`);
  return value as T;
}

function text(tag: string, value: string, className = ""): HTMLElement {
  const node = document.createElement(tag);
  node.textContent = value;
  if (className) node.className = className;
  return node;
}

function statePill(state: unknown): HTMLElement {
  const presentation = presentState(state);
  const node = text("span", presentation.label, `state state-${presentation.tone}`);
  node.title = presentation.description;
  return node;
}

function setEmpty(container: HTMLElement, message: string): void {
  container.replaceChildren(text("p", message, "empty"));
}

function formatTime(value: unknown): string {
  if (typeof value !== "string" || value === "") return "时间未知";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("zh-CN", { hour12: false });
}

function qualityNodes(flags: string[], clockUncertain = false): HTMLElement | null {
  const values = [...new Set([...(flags ?? []), ...(clockUncertain ? ["clock_uncertain"] : [])])];
  if (values.length === 0) return null;
  const list = document.createElement("div");
  list.className = "quality-list";
  list.setAttribute("aria-label", "质量标记");
  for (const flag of values) list.append(text("span", qualityLabel(flag), "quality"));
  return list;
}

function metric(label: string, value: string): HTMLElement {
  const item = document.createElement("div");
  item.className = "metric";
  item.append(text("dt", label), text("dd", value));
  return item;
}

function localDayRange(): { start: string; end: string } {
  const now = new Date();
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
  return { start: start.toISOString(), end: end.toISOString() };
}

async function requestJSON<T>(target: string): Promise<T> {
  const controller = new AbortController();
  const timeout = window.setTimeout(() => controller.abort(), 8000);
  try {
    const response = await fetch(target, { method: "GET", cache: "no-store", signal: controller.signal, headers: { Accept: "application/json" } });
    if (!response.ok) {
      let code = `HTTP_${response.status}`;
      try {
        code = apiErrorCode(response.status, await response.json());
      } catch { /* The stable HTTP status remains visible. */ }
      throw new Error(code);
    }
    return await response.json() as T;
  } finally {
    window.clearTimeout(timeout);
  }
}

function renderStorage(summary: DashboardSummary): void {
  const container = byID("storage-content");
  const grid = document.createElement("dl");
  grid.className = "metric-grid";
  const disk = presentState(summary.operations.disk_level);
  grid.append(
    metric("保护级别", `${disk.label}${summary.operations.error_code ? ` · ${summary.operations.error_code}` : ""}`),
    metric("可用空间", summary.operations.disk_level === "unavailable" ? "未知" : formatBytes(summary.operations.free_bytes)),
    metric("自动保留", presentState(summary.operations.retention).label),
    metric("最后检查", formatTime(summary.operations.checked_at_utc))
  );
  container.replaceChildren(grid);
}

function renderCollectors(summary: DashboardSummary): void {
  const container = byID("collectors-content");
  if (summary.collectors.length === 0) {
    setEmpty(container, "未配置或未启用 ActivityWatch 采集器；不能解释为采集正常。");
    return;
  }
  const list = document.createElement("ul");
  list.className = "status-list";
  for (const collector of summary.collectors) {
    const item = document.createElement("li");
    item.className = "status-row";
    const top = document.createElement("div");
    top.className = "row-top";
    const title = document.createElement("div");
    title.append(text("p", collector.collector_id, "row-title"), text("p", collector.kind, "row-meta"));
    top.append(title, statePill(collector.status));
    item.append(top, text("p", `最近成功：${formatTime(collector.last_success_utc)}${collector.error_code ? ` · ${collector.error_code}` : ""}`, "row-detail"));
    list.append(item);
  }
  container.replaceChildren(list);
}

function renderBacklog(summary: DashboardSummary): void {
  const container = byID("backlog-content");
  const grid = document.createElement("dl");
  grid.className = "metric-grid";
  const mediaCount = summary.media.filesystem_ready_backlog + summary.media.ingest.backlog;
  grid.append(
    metric("媒体入口", formatBacklog(summary.media.status, mediaCount)),
    metric("媒体待处理字节", summary.media.status === "disabled" ? "已关闭" : formatBytes(summary.media.filesystem_ready_bytes)),
    metric("分析模块", formatBacklog(summary.analysis.status, summary.analysis.backlog)),
    metric("外部采集器积压", summary.external_backlogs.length === 0 ? "无已启用采集器" : "未上报")
  );
  container.replaceChildren(grid);
}

function renderFaults(summary: DashboardSummary): void {
  const container = byID("faults-content");
  const faults = summary.history.recent_faults;
  const degraded = summary.history.modules.filter((module) => ["degraded", "disabled", "unavailable"].includes(module.status));
  if (faults.length === 0 && degraded.length === 0) {
    setEmpty(container, "查询成功：最近没有已记录故障或当前降级模块。");
    return;
  }
  const list = document.createElement("ul");
  list.className = "fault-list";
  for (const module of degraded) {
    const item = document.createElement("li");
    item.className = "fault-row";
    const top = document.createElement("div");
    top.className = "row-top";
    top.append(text("p", `${module.module} · ${module.reason_code}`, "row-title"), statePill(module.status));
    item.append(top, text("p", `当前模块状态 · ${formatTime(module.occurred_at_utc)}`, "row-detail"));
    list.append(item);
  }
  for (const fault of faults) {
    const item = document.createElement("li");
    item.className = "fault-row";
    const top = document.createElement("div");
    top.className = "row-top";
    top.append(text("p", `${fault.severity} · ${fault.module} · ${fault.error_code}`, "row-title"), statePill(fault.status));
    item.append(top, text("p", formatTime(fault.occurred_at_utc), "row-detail"));
    list.append(item);
  }
  container.replaceChildren(list);
}

async function loadSummary(): Promise<void> {
  const summary = await requestJSON<DashboardSummary>("/api/v1/dashboard/summary");
  byID("runtime-mode").textContent = `模式：${summary.runtime_mode}`;
  byID("last-updated").textContent = `页面数据生成于 ${formatTime(summary.generated_at_utc)}；不自动轮询。`;
  renderStorage(summary);
  renderCollectors(summary);
  renderBacklog(summary);
  renderFaults(summary);
}

function renderTimeline(): void {
  const container = byID("timeline-content");
  if (timelineEntries.length === 0) {
    setEmpty(container, "查询成功：今天暂时没有时间线 Evidence。无数据不等于采集正常。");
  } else {
    const list = document.createElement("ol");
    list.className = "timeline-list";
    for (const entry of timelineEntries) {
      const item = document.createElement("li");
      item.className = "timeline-row";
      const top = document.createElement("div");
      top.className = "row-top";
      const heading = document.createElement("div");
      heading.append(
        text("p", `${entry.collector_id} · ${entry.event_type || entry.source_type}`, "row-title"),
        text("p", `校正时间 ${formatTime(entry.corrected_start_utc)} · 接收 ${formatTime(entry.received_at_utc)}`, "row-meta")
      );
      top.append(heading, statePill(entry.clock_uncertain ? "unknown" : "covered"));
      item.append(top, text("p", `设备原始时间：${entry.device_start_raw || "未知"} · 时钟误差 ±${entry.clock_error_ms} ms`, "row-detail"));
      const qualities = qualityNodes(entry.quality_flags, entry.clock_uncertain);
      if (qualities) item.append(qualities);
      list.append(item);
    }
    container.replaceChildren(list);
  }
  const more = byID<HTMLButtonElement>("timeline-more");
  more.hidden = timelineCursor === "" || timelineEntries.length >= timelineMaximum;
  if (timelineEntries.length >= timelineMaximum && timelineCursor !== "") {
    container.append(text("p", `已达到页面上限 ${timelineMaximum} 条；缩小时间范围后再查询。`, "row-detail"));
  }
}

async function loadTimeline(reset: boolean): Promise<void> {
  if (reset) {
    timelineEntries = [];
    timelineCursor = "";
  }
  const remaining = timelineMaximum - timelineEntries.length;
  if (remaining <= 0) return;
  const limit = Math.min(timelineLimit, remaining);
  const query = new URLSearchParams({ start: dayStart, end: dayEnd, limit: String(limit) });
  if (timelineCursor) query.set("cursor", timelineCursor);
  const page = await requestJSON<TimelinePage>(`/api/v1/timeline?${query.toString()}`);
  timelineEntries.push(...(page.entries ?? []));
  timelineCursor = page.next_cursor ?? "";
  renderTimeline();
}

async function loadCoverage(): Promise<void> {
  const response = await requestJSON<CoverageResponse>(`/api/v1/coverage?${new URLSearchParams({ start: dayStart, end: dayEnd }).toString()}`);
  const container = byID("coverage-content");
  if ((response.projections ?? []).length === 0) {
    setEmpty(container, "没有启用的采集器覆盖投影；不能解释为 100% 覆盖。");
    return;
  }
  const wrapper = document.createElement("div");
  wrapper.className = "table-wrap";
  const table = document.createElement("table");
  const caption = document.createElement("caption");
  caption.textContent = "今日采集器覆盖区间";
  caption.className = "empty";
  const head = document.createElement("thead");
  const headRow = document.createElement("tr");
  for (const label of ["采集器", "起止", "可用性", "质量与原因"]) headRow.append(text("th", label));
  head.append(headRow);
  const body = document.createElement("tbody");
  for (const interval of (response.intervals ?? []).slice(0, coverageMaximum)) {
    const row = document.createElement("tr");
    const stateCell = document.createElement("td");
    stateCell.append(statePill(interval.availability));
    const quality = (interval.quality_flags ?? []).map(qualityLabel).join("、") || "无质量标记";
    row.append(
      text("td", interval.collector_id),
      text("td", `${formatTime(interval.start_utc)} — ${formatTime(interval.end_utc)}`),
      stateCell,
      text("td", `${quality} · ${interval.reason_code}`)
    );
    body.append(row);
  }
  table.append(caption, head, body);
  wrapper.append(table);
  container.replaceChildren(wrapper);
  if ((response.intervals ?? []).length > coverageMaximum) container.append(text("p", `仅显示前 ${coverageMaximum} 个区间。`, "row-detail"));
}

function describeError(error: unknown): string {
  if (error instanceof DOMException && error.name === "AbortError") return "读取超过 8 秒，已停止等待；采集继续运行。";
  if (error instanceof Error) return error.message;
  return "未知读取错误";
}

async function refreshAll(): Promise<void> {
  const refresh = byID<HTMLButtonElement>("refresh");
  const errorBanner = byID("page-error");
  refresh.disabled = true;
  errorBanner.hidden = true;
  const range = localDayRange();
  dayStart = range.start;
  dayEnd = range.end;
  const failures: string[] = [];
  try { await loadSummary(); } catch (error) {
    failures.push(`状态摘要：${describeError(error)}`);
    for (const id of ["storage-content", "collectors-content", "backlog-content", "faults-content"]) setEmpty(byID(id), "状态不可用，不能按正常处理。");
    byID("runtime-mode").textContent = "模式：未知";
  }
  try { await loadTimeline(true); } catch (error) {
    failures.push(`时间线：${describeError(error)}`);
    setEmpty(byID("timeline-content"), "时间线读取失败；原始 Evidence 和采集不受影响。");
    byID<HTMLButtonElement>("timeline-more").hidden = true;
  }
  try { await loadCoverage(); } catch (error) {
    failures.push(`覆盖率：${describeError(error)}`);
    setEmpty(byID("coverage-content"), "覆盖率读取失败；不能把缺失解释为空闲或正常。");
  }
  if (failures.length !== 0) {
    errorBanner.textContent = failures.join("；");
    errorBanner.hidden = false;
  }
  refresh.disabled = false;
}

byID<HTMLButtonElement>("refresh").addEventListener("click", () => { void refreshAll(); });
byID<HTMLButtonElement>("timeline-more").addEventListener("click", async (event) => {
  const button = event.currentTarget as HTMLButtonElement;
  button.disabled = true;
  try { await loadTimeline(false); } catch (error) {
    const banner = byID("page-error");
    banner.textContent = `时间线下一页：${describeError(error)}`;
    banner.hidden = false;
  } finally { button.disabled = false; }
});

void refreshAll();
