//go:build windows

package tools

import (
	"syscall"
	"time"
)

// processCPUTime 返回当前进程已消耗的 CPU 时间（user+kernel）。
// Windows 用 GetProcessTimes（FILETIME，100ns 单位）；失败时返回 ok=false，
// 调用方只记录 elapsed（观测降级，不影响主流程）。
func processCPUTime() (time.Duration, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return time.Duration(kernel.Nanoseconds()+user.Nanoseconds()) * time.Nanosecond, true
}