//go:build !windows && !unix

package tools

import "time"

// processCPUTime 在无进程 CPU 时间 API 的平台（plan9/js 等）返回 ok=false，
// 调用方只记录 elapsed（观测降级，不影响主流程）。
func processCPUTime() (time.Duration, bool) { return 0, false }