//go:build unix

package tools

import (
	"syscall"
	"time"
)

// processCPUTime 返回当前进程已消耗的 CPU 时间（user+kernel）。
// Unix 用 getrusage(RUSAGE_SELF)；失败时返回 ok=false，调用方只记录 elapsed
// （观测降级，不影响主流程）。
func processCPUTime() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano()+ru.Stime.Nano()) * time.Nanosecond, true
}