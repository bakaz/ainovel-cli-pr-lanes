## 摘要

抽出 `buildWorkerToolsets`，让 Engine 各 worker 共用同一套生产工具组合，并用契约测试锁住列表，避免接线漂移。

## 依赖

- Style 系列：规则 → Compass → 锚点

## 主要改动

- `internal/agents/build.go`：`workerToolsets` + `buildWorkerToolsets`
- `TestBuildWorkers_ToolComposition` 契约测试
- Host 侧测试夹具适配分层 `StoryCompass`

## 非目标

- 工具超时中间件 / `wrapTools`
- Recovery 调度器

## 隐私

- 无用户数据、无密钥

## 测试计划

- [ ] `go test ./internal/agents/... ./internal/host/... -count=1`
- [ ] `architect_short` 与 `architect_long` 工具列表不同（仅 long 含 `read_planning_reference`）
- [ ] architect / writer / editor 使用**不同**的 `novel_context` 实例
