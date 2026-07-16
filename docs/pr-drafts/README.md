# 向 voocel/ainovel-cli 提交的 Draft PR

工作区：`G:\opencode\ainovel-pr-prep`  
上游：`upstream` → `voocel/ainovel-cli`  
推送：`origin` → `bakaz/ainovel-cli`

## 隐私与敏感信息检查（已扫分支 diff）

| 项 | 结论 |
|----|------|
| 真实 API Key / Token | **无**。observe 测试里仅有假值 `sk-test` |
| 本机绝对路径（`C:\Users\...`、`G:\opencode\...`） | **无**写入提交内容 |
| 真实书稿 / 私人小说正文 | **无**；锚点测试用虚构短句 |
| 配置文件、`.env` | **无** |
| 提交作者邮箱 | 提交为 `bakaz zhang <zybdnf@gmail.com>`，**开 PR 后会公开**。若不想暴露，push 前改 `user.email` 并 `git rebase` 改写作者（仅限未推送分支） |

### 产品层信任边界（不是泄漏，但建议写进相关 PR）

1. **overlay（#6）**：会加载 `~/.ainovel`、`./.ainovel`、`--prompts-dir` 下的本地 prompt，属于**可信本地配置**，可改变模型行为。
2. **observe（#7）**：读取本机用户配置里的 provider 凭证做探活；**不把密钥写进仓库**，但会向已配置的 provider 发一次请求。
3. **文风锚点（#4）**：用户可在 `meta/style_anchors.json` 放入 excerpt；运行时会注入上下文并可能发给模型。PR 内只有测试假数据。

## 分支与文案

| 顺序 | 分支 | 文案 | 说明 |
|-----:|------|------|------|
| 1 | `pr/windows-portability` | `01-windows.md` | 独立 |
| 2 | `pr/feat-rules` | `02-rules.md` | Style 系列起点 |
| 3 | `pr/feat-compass` | `03-compass.md` | 叠在 #2 |
| 4 | `pr/feat-anchors` | `04-anchors.md` | 叠在 #3 |
| 5 | `pr/feat-workers` | `05-workers.md` | 叠在 #4 |
| 6 | `pr/assets-overlay` | `06-overlay.md` | 独立 |
| 7 | `pr/observe-probe` | `07-observe.md` | 独立 |
| 备用 | `pr/style-quality-clean` | （可合并 02–05） | 单 PR 备选 |

**暂缓：** 工具超时。

## 建议开 PR 顺序

1. 可并行：`#1`、`#6`、`#7`
2. Style 串行：`#2` 合入 → rebase `#3` → … → `#5`

## 推送示例

```powershell
cd G:\opencode\ainovel-pr-prep
git push -u origin pr/windows-portability
gh pr create --repo voocel/ainovel-cli --base main --head bakaz:pr/windows-portability `
  --draft --title "fix(windows): 通知与更新器测试兼容 Windows" `
  --body-file .pr-drafts/01-windows.md
```
