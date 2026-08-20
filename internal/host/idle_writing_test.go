package host

import (
	"testing"
	"time"
)

func TestIdleWritingStatusAtBeijingPeakBoundaries(t *testing.T) {
	loc := time.FixedZone(IdleWritingTimezone, 8*60*60)
	cases := []struct {
		name     string
		at       time.Time
		inPeak   bool
		nextHour int
		nextDay  int
	}{
		{name: "before morning peak", at: time.Date(2026, 8, 20, 8, 59, 59, 0, loc), nextHour: 9, nextDay: 20},
		{name: "morning peak start", at: time.Date(2026, 8, 20, 9, 0, 0, 0, loc), inPeak: true, nextHour: 12, nextDay: 20},
		{name: "morning peak end", at: time.Date(2026, 8, 20, 12, 0, 0, 0, loc), nextHour: 14, nextDay: 20},
		{name: "afternoon peak start", at: time.Date(2026, 8, 20, 14, 0, 0, 0, loc), inPeak: true, nextHour: 18, nextDay: 20},
		{name: "afternoon peak end", at: time.Date(2026, 8, 20, 18, 0, 0, 0, loc), nextHour: 9, nextDay: 21},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := IdleWritingStatusAt(tc.at)
			if status.InPeak != tc.inPeak {
				t.Fatalf("InPeak = %v, want %v", status.InPeak, tc.inPeak)
			}
			if status.NextTransition.Hour() != tc.nextHour || status.NextTransition.Day() != tc.nextDay {
				t.Fatalf("next transition = %s, want day %d hour %d", status.NextTransition, tc.nextDay, tc.nextHour)
			}
			if status.BeijingNow.Location().String() != IdleWritingTimezone {
				t.Fatalf("location = %q, want %q", status.BeijingNow.Location(), IdleWritingTimezone)
			}
		})
	}
}

func TestIdleWritingStatusAtConvertsToBeijing(t *testing.T) {
	// 01:00 UTC = 09:00 北京时间，必须落入上午高峰。
	status := IdleWritingStatusAt(time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC))
	if !status.InPeak {
		t.Fatal("01:00 UTC should be 09:00 Beijing and in peak")
	}
	if status.BeijingNow.Hour() != 9 || status.NextTransition.Hour() != 12 {
		t.Fatalf("BeijingNow=%s next=%s", status.BeijingNow, status.NextTransition)
	}
}
