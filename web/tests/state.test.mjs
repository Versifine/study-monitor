import test from "node:test";
import assert from "node:assert/strict";
import { apiErrorCode, formatBacklog, presentState, qualityLabel } from "../dist/assets/state.js";

test("maps absence and degraded states without treating them as normal", () => {
  assert.equal(presentState("disabled").label, "已关闭");
  assert.equal(presentState("not_installed").label, "未安装");
  assert.equal(presentState("offline").label, "离线");
  assert.equal(presentState("delayed").label, "延迟");
  assert.equal(presentState("unknown").label, "未知");
  assert.equal(presentState(undefined).label, "无数据");
  assert.equal(presentState("enabled").label, "已启用");
  assert.notEqual(presentState("offline").tone, "good");
});

test("distinguishes zero backlog from disabled, not installed, and unreported", () => {
  assert.equal(formatBacklog("disabled", 0), "已关闭（不显示为 0）");
  assert.equal(formatBacklog("not_installed", 0), "未安装（无分析队列）");
  assert.equal(formatBacklog("healthy", null), "未上报");
  assert.equal(formatBacklog("healthy", 0), "0（已确认无积压）");
  assert.equal(formatBacklog("healthy", 12), "12 条");
});

test("uses text labels for quality rather than color alone", () => {
  assert.equal(qualityLabel("clock_uncertain"), "时钟不确定");
  assert.equal(qualityLabel("incomplete"), "不完整");
});

test("keeps stable API error codes visible", () => {
  assert.equal(apiErrorCode(503, { error: { code: "STORE_QUERY_FAILED" } }), "STORE_QUERY_FAILED");
  assert.equal(apiErrorCode(503, { error_code: "legacy-wrong-shape" }), "HTTP_503");
});
