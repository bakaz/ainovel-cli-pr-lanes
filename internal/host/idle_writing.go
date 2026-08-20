package host

import "time"

// IdleWritingTimezone 是闲时写作调度使用的固定时区。
// 中国大陆不使用夏令时，固定 +08:00 比依赖 Windows 本地 tzdata 更稳定。
const IdleWritingTimezone = "Asia/Shanghai"

var idleWritingBeijing = time.FixedZone(IdleWritingTimezone, 8*60*60)

// IdleWritingScheduleStatus 描述某一时刻的北京时间窗口。
type IdleWritingScheduleStatus struct {
	BeijingNow     time.Time
	InPeak         bool
	NextTransition time.Time
}

// IdleWritingStatusAt 计算闲时写作调度窗口。
// 高峰区间采用左闭右开：[09:00,12:00)、[14:00,18:00)，
// 因此 12:00 和 18:00 起即可恢复闲时写作。
func IdleWritingStatusAt(now time.Time) IdleWritingScheduleStatus {
	if now.IsZero() {
		now = time.Now()
	}
	local := now.In(idleWritingBeijing)
	day := func(hour int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, idleWritingBeijing)
	}

	startMorning := day(9)
	endMorning := day(12)
	startAfternoon := day(14)
	endAfternoon := day(18)
	switch {
	case local.Before(startMorning):
		return IdleWritingScheduleStatus{BeijingNow: local, NextTransition: startMorning}
	case local.Before(endMorning):
		return IdleWritingScheduleStatus{BeijingNow: local, InPeak: true, NextTransition: endMorning}
	case local.Before(startAfternoon):
		return IdleWritingScheduleStatus{BeijingNow: local, NextTransition: startAfternoon}
	case local.Before(endAfternoon):
		return IdleWritingScheduleStatus{BeijingNow: local, InPeak: true, NextTransition: endAfternoon}
	default:
		next := startMorning.Add(24 * time.Hour)
		return IdleWritingScheduleStatus{BeijingNow: local, NextTransition: next}
	}
}
