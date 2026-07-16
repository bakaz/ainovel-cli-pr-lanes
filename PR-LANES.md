# ainovel-cli-pr-lanes

面向 [voocel/ainovel-cli](https://github.com/voocel/ainovel-cli) 的**贡献分支暂存仓**（公开）。

本仓库 **不是** 上游 fork 的替代品，只用来：

- 存放已按功能拆好的 PR 分支
- 存放中文 Draft PR 文案
- 方便从干净 head 向 `voocel/ainovel-cli` 开 PR

上游代码基线：`main` ≈ `voocel/ainovel-cli@main`（创建时同步）。

## 分支一览

| 分支 | 用途 | 建议 PR 标题 |
|------|------|----------------|
| `pr/windows-portability` | Windows 测试兼容 | `fix(windows): 通知与更新器测试兼容 Windows` |
| `pr/feat-rules` | 角色化写作偏好 | `feat(rules): 写作偏好按角色分桶` |
| `pr/feat-compass` | 分层 Compass（叠在 rules 上） | `feat(planning): 分层 Compass 与按需规划引用` |
| `pr/feat-anchors` | 文风锚点（叠在 compass 上） | `feat(style): 手动文风锚点与风格上下文保护` |
| `pr/feat-workers` | Worker 工具组合（叠在 anchors 上） | `refactor(workers): 统一 Engine worker 工具组合` |
| `pr/assets-overlay` | 生产 prompt overlay | `feat(assets): 生产环境 prompt/参考资料 overlay` |
| `pr/observe-probe` | provider 探活 | `feat(cli): 只读 provider 探活子命令` |
| `pr/style-quality-clean` | Style 整包备选（单 PR） | 合并 02–05 时用 |
| `prep/style-source` | 拆分用的中间源 | 勿直接开 PR |

## 建议开 PR 顺序

1. 可并行：`pr/windows-portability`、`pr/assets-overlay`、`pr/observe-probe`
2. Style 串行：`feat-rules` → 合入上游后 rebase → `feat-compass` → … → `feat-workers`

向下游开 PR 时：

```text
base:  voocel/ainovel-cli:main
head:  bakaz/ainovel-cli-pr-lanes:<branch>
```

## Draft 文案

见 [`docs/pr-drafts/`](docs/pr-drafts/)。

## 隐私说明

- 分支 diff 已扫过：无真实 API Key、无本机绝对路径、无真实书稿
- 提交作者邮箱会随 commit 公开；如需隐藏请在 push 前改写作者
- 产品信任边界见 `docs/pr-drafts/README.md`

## 与 bakaz/ainovel-cli 的关系

| 仓库 | 角色 |
|------|------|
| [voocel/ainovel-cli](https://github.com/voocel/ainovel-cli) | 上游 |
| [bakaz/ainovel-cli](https://github.com/bakaz/ainovel-cli) | 个人 fork / 日常开发 |
| **本仓库** | 仅放拆分后的 PR 车道与文案 |
