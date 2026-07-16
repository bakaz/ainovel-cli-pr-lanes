## 摘要

让通知与自更新相关的**单元测试**在 Windows 上稳定通过，不改动生产逻辑。

## 背景

在 Windows 上跑测试时，PowerShell 管道编码、文件权限断言等与 Unix 环境不一致，容易误报失败。这些差异来自测试环境，不是运行时功能 bug。

## 改动

- `internal/notify/notify_test.go`：兼容 Windows 下通知相关编码/管道行为
- `internal/version/update_test.go`：权限相关断言改为平台安全写法

## 非目标

- 不改通知实现、不改更新器实现
- 不引入新功能

## 隐私

- 无密钥、无本机路径、无用户数据

## 测试计划

- [ ] Windows：`go test ./internal/notify ./internal/version -count=1`
- [ ] （如有）Linux/macOS 同包测试仍通过
