package host

import (
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestStartupResumeBlocked(t *testing.T) {
	tests := []struct {
		name     string
		meta     *domain.RunMeta
		progress *domain.Progress
		want     bool
	}{
		{
			name:     "ordinary writing resumes",
			meta:     &domain.RunMeta{AdvanceMode: domain.ChapterAdvanceAuto},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     false,
		},
		{
			name: "session exit resumes",
			meta: &domain.RunMeta{
				AdvanceMode: domain.ChapterAdvanceAuto,
				Control: &domain.RunControl{LastStop: &domain.RunStopRecord{
					Category: domain.StopCategorySessionExit,
				}},
			},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     false,
		},
		{
			name:     "pending intervention blocks",
			meta:     &domain.RunMeta{PendingSteer: "请重写第3章"},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     true,
		},
		{
			name:     "advance hold blocks",
			meta:     &domain.RunMeta{AdvanceHold: &domain.AdvanceHold{After: domain.AdvanceHoldAtBoundary, Reason: "验收"}},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     true,
		},
		{
			name:     "review mode blocks",
			meta:     &domain.RunMeta{AdvanceMode: domain.ChapterAdvanceReview},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     true,
		},
		{
			name:     "pending rewrites remain resumable",
			meta:     &domain.RunMeta{AdvanceMode: domain.ChapterAdvanceAuto},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowRewriting, CurrentChapter: 3, PendingRewrites: []int{2}},
			want:     false,
		},
		{
			name:     "steering flow blocks",
			meta:     &domain.RunMeta{AdvanceMode: domain.ChapterAdvanceAuto},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowSteering, CurrentChapter: 3},
			want:     true,
		},
		{
			name: "known error stop blocks",
			meta: &domain.RunMeta{
				Control: &domain.RunControl{LastStop: &domain.RunStopRecord{
					Category: domain.StopCategoryDecisionFailed,
				}},
			},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     true,
		},
		{
			name: "unclean active run blocks",
			meta: &domain.RunMeta{
				Control: &domain.RunControl{Active: true},
			},
			progress: &domain.Progress{Phase: domain.PhaseWriting, Flow: domain.FlowWriting, CurrentChapter: 3},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startupResumeBlocked(tt.meta, tt.progress); got != tt.want {
				t.Fatalf("startupResumeBlocked() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartupResumeDeferredDuringConfiguredPeak(t *testing.T) {
	loc := time.FixedZone(IdleWritingTimezone, 8*60*60)
	peak := time.Date(2026, 8, 21, 10, 0, 0, 0, loc)
	open := time.Date(2026, 8, 21, 13, 0, 0, 0, loc)

	for _, tc := range []struct {
		name string
		meta *domain.RunMeta
		at   time.Time
		want bool
	}{
		{name: "idle writing defers in peak", meta: &domain.RunMeta{IdleWritingEnabled: true}, at: peak, want: true},
		{name: "peak policy defers in peak", meta: &domain.RunMeta{PeakAutoPauseEnabled: true}, at: peak, want: true},
		{name: "open window does not defer", meta: &domain.RunMeta{IdleWritingEnabled: true}, at: open, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := startupResumeDeferred(tc.meta, tc.at); got != tc.want {
				t.Fatalf("startupResumeDeferred() = %v, want %v", got, tc.want)
			}
		})
	}
}
