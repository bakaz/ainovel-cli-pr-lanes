## 摘要

将 `StoryCompass` 拆成稳定的 **long** 与滚动的 **current**，并新增 `read_planning_reference`，让长篇 Architect **按需**拉取远期规划细节，而不是每轮全量注入。

## 依赖

- 前置 PR：角色化写作偏好（`pr/feat-rules`）

## 背景

把整份长线规划塞进每一次 Architect 调用，成本高、噪声大。结局方向、开放线索等应长期稳定；近端「当前罗盘」可以滚动更新。

## 主要改动

- `StoryCompass{Long, Current}` + 旧根形状 JSON 迁移
- `save_foundation(update_compass)` 支持 `section=long|current`
- 新工具 `read_planning_reference`（批量 long 参考 + 卷详情；单次最多 6 卷）
- `architect-long` prompt：规划引用调用次数建议（≤2）
- `novel_context` 分层 compass 视图
- 完本相关检查改为使用 `compass.Long.OpenThreads`

## 非目标

- 文风锚点 / style_rules 分层持久化（下一 PR）
- Worker 工具表重构（更后 PR）
- 工具超时

## 隐私

- 无真实书目规划入库
- 工具只读用户本地书目元数据，不新增外传通道

## 测试计划

- [ ] `go test ./internal/domain/ ./internal/tools/ ./internal/store/ ./internal/diag/ ./internal/agents/ -count=1`
- [ ] 旧 `meta/compass.json` 根形状可迁移
- [ ] `architect_long` 含 `read_planning_reference`，`architect_short` 不含
