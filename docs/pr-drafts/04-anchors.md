## 摘要

增加手动文风锚点（`meta/style_anchors.json`）与分层 style rules compass；在上下文压缩时**优先保留** `style_rules` 与手动锚点。

## 依赖

- 角色化写作偏好
- 分层 Compass + 按需规划

## 背景

写手需要可复用的「笔法校准」，但不希望把长样本整段贴进每章 prompt。手动锚点应高于自动风格信号，并在预算裁剪时尽量不被丢掉。

## 主要改动

- 领域模型：`StyleAnchorsV1`、`WritingStyleRulesCompass`（long/current）
- 存储：`StyleAnchorsStore.LoadManual`（状态机）、style rules compass 读写 API
- `novel_context`：注入 `style_anchors_manual` / `style_anchors_auto`；手动锚点不计入软预算度量
- Writer / Editor prompt：只学**抽象叙事特征**，禁止抄 excerpt / 复现样本剧情与专名
- 更新 `assets/testdata/writer-golden.md`

## 优先级（高 → 低）

事实/canon、章节契约 → 生效中的 user_rules → **manual anchors** → style_rules → auto anchors

## 非目标

- 管理锚点的 CLI/TUI（v1 为手改 JSON）
- 工具超时、资源 overlay

## 隐私与信任边界

- **仓库内**：仅测试用虚构 excerpt（如「夜色如墨…」），无真实书稿
- **运行时**：用户若在 `meta/style_anchors.json` 写入正文片段，会进入模型上下文；请勿放入隐私或未授权文本
- 文件有大小/条数上限（如单文件约 64KiB、excerpt 长度限制），降低误塞超大内容的风险

## 测试计划

- [ ] `go test ./internal/domain/ ./internal/store/ ./internal/tools/ ./assets -count=1`
- [ ] 校验锚点条数/长度上限
- [ ] 确认 trim 后仍保留 `style_rules` 与 `style_anchors_manual`
