# M5 最小只读仪表盘

## 1. 目的与边界

仪表盘只用于快速确认 Recorder Core 的事实、覆盖缺口和运行健康。它不提供修改 Evidence、学习计划、保留策略、采集器、运行模式、模型状态或备份/回滚的入口，也不运行分析任务。关闭页面、浏览器卡顿、资源缺失或汇总查询失败不得影响采集写入、健康检查、恢复和运维脚本。

页面固定展示六类信息：

1. 磁盘与保护
2. 采集器状态
3. 媒体、分析与外部来源积压
4. 当日本地自然日覆盖率与质量标记
5. 当日 Evidence 时间线与时钟不确定性
6. 最近故障、恢复、模块状态与降级原因

## 2. 只读数据合同

- 静态资源：`GET|HEAD /`、`/index.html`、`/assets/styles.css`、`/assets/state.js`、`/assets/app.js`。
- 汇总：`GET|HEAD /api/v1/dashboard/summary`，最近故障最多 20 条、每个模块只返回最新状态，故障详情不返回浏览器。
- 时间线：`GET|HEAD /api/v1/timeline`，每页 50 条，前端单次刷新最多读取 200 条。
- 覆盖率：`GET|HEAD /api/v1/coverage`，前端单次最多渲染 200 个区间。
- 页面依次读取汇总、时间线和覆盖率，不并发占用时间线/覆盖率的单投影门，不自动轮询；每个请求 8 秒后在浏览器端放弃等待。

所有响应保持 `Cache-Control: no-store`。静态处理器使用资源白名单和 CSP，拒绝 POST 等非 GET/HEAD 方法。M5 不增加数据库迁移、写 API、独立前端服务或运行时队列。

## 3. 状态语义

| 数据 | 显示 | 禁止解释为 |
|---|---|---|
| `disabled` | 已关闭 | 正常、零积压 |
| `not_installed` | 未安装，无分析队列 | 0 条待分析 |
| `offline` | 离线 | 空闲 |
| `delayed` | 延迟 | 最新 |
| `unknown` 或缺字段 | 未知/无数据 | 正常 |
| 已上报数值 `0` | 0（已确认无积压） | 未上报 |
| 外部来源未提供积压合同 | 未上报，数值为 `null` | 0 |

颜色只辅助分组；每个状态同时提供文字、稳定状态值或错误码。时间线显示 `clock_uncertain`，覆盖区间显示 `obscured`、`corrupt`、`clock_uncertain`、`incomplete` 等文字质量标记。页头始终显示 `record-only` 或当前降级模式及数据生成时间。

## 4. 构建与运行

`web/package.json` 固定 Node.js `24.11.1`，依赖和开发依赖均为空。`web/build.mjs` 只使用该版本 Node.js 内置的 TypeScript 类型擦除，将 `web/src` 确定性生成到 `web/dist`；`--check` 拒绝缺失、陈旧或额外产物。Go 的 `web/assets.go` 嵌入已提交的 `dist`，因此生产二进制不需要 Node.js 或网络。

```powershell
# 修改前端源码后重建
.\scripts\build-web.ps1

# 验证提交的产物与源码完全一致
.\scripts\build-web.ps1 -Check

# 状态映射、Go 契约与脚本测试
.\scripts\test.ps1
```

`EXAM_MONITOR_DASHBOARD_ENABLED=false` 可在不改变 JSON 配置 schema 的情况下关闭仪表盘。默认 Record-only 启用；Minimum mode 始终关闭。关闭或嵌入资源校验失败时不注册根页面和汇总路由，核心 API 与运维行为保持不变。

## 5. 可访问性与浏览器门槛

- 使用系统字体，不加载外部资源或建立第三方连接。
- 所有交互可用键盘操作，焦点可见；刷新按钮触控高度至少 44px。
- 375、768、1024、1440 像素宽度无横向溢出；窄屏退化为单列。
- 支持 `prefers-reduced-motion`，不包含复杂动画。
- smoke 必须从候选二进制打开嵌入页面，验证六类面板、GET-only 资源和显式未知/零值语义。
