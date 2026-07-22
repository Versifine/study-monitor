







const states                                    = {
  healthy: { tone: "good", label: "正常", description: "最近一次检查正常" },
  normal: { tone: "good", label: "正常", description: "未触发保护水位" },
  writable: { tone: "good", label: "可写", description: "核心存储可以确认写入" },
  enabled: { tone: "info", label: "已启用", description: "该功能已由配置明确启用" },
  covered: { tone: "good", label: "已覆盖", description: "该时段存在有效证据" },
  confirmed_idle: { tone: "info", label: "已确认空闲", description: "采集器明确报告空闲" },
  recovered: { tone: "good", label: "已恢复", description: "故障恢复事实已记录" },
  fresh: { tone: "good", label: "最新", description: "投影已按当前事实重建" },
  warning: { tone: "warning", label: "预警", description: "已暂停低优先级媒体工作" },
  pending: { tone: "warning", label: "待结算", description: "仍在允许迟到窗口内" },
  delayed: { tone: "warning", label: "延迟", description: "数据晚于预期但尚未判定离线" },
  backlogged: { tone: "warning", label: "有积压", description: "存在明确上报的待处理项" },
  stale: { tone: "warning", label: "陈旧", description: "投影未能按最新事实完成" },
  critical: { tone: "danger", label: "关键", description: "已拒绝新媒体以保护核心存储" },
  reserve: { tone: "danger", label: "保留空间受威胁", description: "核心写入不会被确认" },
  unavailable: { tone: "danger", label: "不可用", description: "模块存在明确故障" },
  offline: { tone: "danger", label: "离线", description: "已超过采集器离线阈值" },
  corrupt: { tone: "danger", label: "损坏", description: "证据未通过完整性验证" },
  active: { tone: "danger", label: "活动故障", description: "故障尚未恢复" },
  degraded: { tone: "warning", label: "降级", description: "模块仍运行但能力受限" },
  disabled: { tone: "muted", label: "已关闭", description: "模块被明确关闭，不等于正常或零积压" },
  not_installed: { tone: "muted", label: "未安装", description: "该模块没有安装，也没有待处理队列" },
  unknown: { tone: "unknown", label: "未知", description: "当前没有足够证据判断状态" }
};

export function presentState(value         )                    {
  if (typeof value !== "string" || value.trim() === "") {
    return { tone: "unknown", label: "无数据", description: "接口没有返回状态" };
  }
  return states[value] ?? { tone: "unknown", label: value, description: "未识别的状态值，不能按正常处理" };
}

export function formatBacklog(moduleState         , count         )         {
  if (moduleState === "disabled") return "已关闭（不显示为 0）";
  if (moduleState === "not_installed") return "未安装（无分析队列）";
  if (typeof count !== "number" || !Number.isFinite(count) || count < 0) return "未上报";
  if (count === 0) return "0（已确认无积压）";
  return `${Math.trunc(count).toLocaleString("zh-CN")} 条`;
}

export function formatBytes(value         )         {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return "未知";
  if (value === 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

export function qualityLabel(value        )         {
  const labels                         = {
    obscured: "遮挡",
    corrupt: "损坏",
    clock_uncertain: "时钟不确定",
    incomplete: "不完整"
  };
  return labels[value] ?? value;
}

export function apiErrorCode(httpStatus        , payload         )         {
  if (payload && typeof payload === "object" && "error" in payload) {
    const error = (payload                       ).error;
    if (error && typeof error === "object" && "code" in error) {
      const code = (error                      ).code;
      if (typeof code === "string" && code !== "") return code;
    }
  }
  return `HTTP_${httpStatus}`;
}
