## 摘要

为写作偏好增加**按角色分桶**（`default` / `architect` / `writer` / `editor`），兼容旧版单字符串格式，并让 `novel_context`、一致性检查按角色注入对应规则视图。

## 背景

长篇创作里，规划师、写手、审阅者需要的「站立规则」并不相同。原先一份自由文本偏好会灌给所有角色，既浪费上下文，也不利于结构化机械检查（例如章节字数区间）。

## 主要改动

- `internal/rules`：`PreferenceBuckets`、范围 CRUD、旧格式迁移
- `Check(text, wordCount, structured)`，支持可选 `chapter_words` 偏离检查
- `Snapshot.PayloadForRole(role)` 生成角色视图
- `userrules` 归一化与 patch（增删改/重分类/重建）支持分桶
- `NewContextToolForRole`；Engine worker 按角色挂不同 `novel_context`
- Editor prompt：说明 Host 拼装 default+writer+editor 视图

## 角色视图

| 角色 | 看到的 preferences |
|------|-------------------|
| Architect | default + architect |
| Writer | default + writer |
| Editor | default + writer + editor |

旧版单一字符串会迁入 `default`，避免已有书目损坏。

## 非目标

- 分层 Compass / 按需规划引用
- 文风锚点
- 资源 overlay、`observe`、工具超时

## 隐私

- 无真实用户规则样例入库；测试数据为虚构
- 运行时用户规则仍只存在于用户书目目录，本 PR 不上传、不外发配置

## 测试计划

- [ ] `go test ./internal/rules/... ./internal/userrules/... ./internal/tools/ -count=1`
- [ ] `go build ./internal/...`
- [ ] 用旧版 `user_rules` JSON 打开书目，确认迁入 `default`
