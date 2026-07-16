## 摘要

支持生产环境的 prompt / 参考资料 **overlay 覆盖**，优先级清晰，并做 UTF-8 校验与来源记录。

## 背景

运维与作者有时需要在不重新编译的前提下覆盖内置 prompt。需要可预期的覆盖顺序，以及无效文件时的安全回退。

## 覆盖优先级（低 → 高）

1. 内置 embed  
2. `~/.ainovel`  
3. `./.ainovel`（当前工作目录 / 书目侧）  
4. `--prompts-dir`（显式指定）

## 主要改动

- `assets/overlay.go` 及测试
- `assets.LoadProduction` 入口与 applied source 报告
- CLI：`--prompts-dir`，启动时走 `LoadProduction`

## 非目标

- `observe` 子命令（另 PR）
- 热重载
- 改变 Arbiter / simulation 包装语义

## 隐私与安全

- **不读取、不提交** API Key
- Overlay 目录视为**可信本地配置**：其中的 prompt 可改变模型行为，请勿放入不可信来源
- 无效 UTF-8 等会告警/回退，避免半截二进制进 prompt
- diff 中无本机绝对路径、无真实用户文件内容

## 测试计划

- [ ] `go test ./assets/... -count=1`
- [ ] `go build ./cmd/ainovel-cli`
- [ ] `--prompts-dir` 不存在或指向文件时，错误信息明确
