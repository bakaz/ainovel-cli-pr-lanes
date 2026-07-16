## 摘要

新增只读的 **provider 探活** 子命令：

```text
ainovel-cli observe --dir <书目目录> [--timeout <时长>]
```

在真实书目目录上做有界探测：确认已配置的模型能吐出首个有效 delta，**不写书目、不调写作工具**。

## 背景

需要一种比完整 Engine 跑书更轻的检查，用于确认本机配置的 provider 是否可用。  
说明：这不是 Coordinator/Engine 状态转储，也不是小说质量评估。

## 行为

- 加载真实用户配置（非空 stub）
- Preflight：目录需像合法书目；探测过程不写入目标树
- 模型调用有超时边界；工具一律拒绝（`DefaultDenyPolicy`）
- 成功条件：超时内收到首个可用模型输出

## 非目标

- 不是 Coordinator / Engine 健康总览
- 不读入完整小说上下文做质量判断
- 本 PR 不含 diag / steer 调试面

## 命名

当前子命令名为 `observe`。若维护者认为 `probe` 更贴切，review 时可改名。

## 隐私与安全

| 点 | 说明 |
|----|------|
| 密钥 | 使用本机已有配置中的 provider 凭证；**不会**把 Key 写入仓库或日志（测试仅用假值 `sk-test`） |
| 书目 | 只读校验目录结构；测试用 `t.TempDir()`，无真实书稿 |
| 外发 | 会向用户已配置的 provider 发一次探活请求（与正常写作相同的信任模型） |
| 工具 | 强制拒绝 tool call，降低误写风险 |

## 测试计划

- [ ] `go test ./internal/observe/... -count=1`
- [ ] `go test -run '^TestObserve' ./cmd/ainovel-cli/... -count=1`
- [ ] （可选）对本机真实书目：`observe --dir <path>` 联调 provider
