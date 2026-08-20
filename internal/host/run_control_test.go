package host

import (
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestScheduledResumeAllowedSeparatesManualAndIdleOrigins(t *testing.T) {
	loc := time.FixedZone(IdleWritingTimezone, 8*60*60)
	peak := time.Date(2026, 8, 20, 10, 0, 0, 0, loc)
	open := time.Date(2026, 8, 20, 13, 0, 0, 0, loc)

	manual := &domain.RunMeta{
		PeakAutoPauseEnabled: true,
		Control:              &domain.RunControl{},
	}
	permit := domain.ResumePermit{Generation: 1, Origin: domain.RunOriginManual, Trigger: domain.ResumeTriggerAfterPeak}
	if (&Host{}).scheduledResumeAllowed(manual, permit, peak) {
		t.Fatal("manual task must wait during peak while peak pause is enabled")
	}
	manual.PeakAutoPauseEnabled = false
	if !(&Host{}).scheduledResumeAllowed(manual, permit, peak) {
		t.Fatal("manual task should be allowed during peak after permanent peak pause is disabled")
	}
	manual.PeakAutoPauseEnabled = true
	manual.Control.PeakOverrideUntil = peak.Add(30 * time.Minute).Format(time.RFC3339)
	if !(&Host{}).scheduledResumeAllowed(manual, permit, peak) {
		t.Fatal("manual task should be allowed during the active skip override")
	}

	idle := &domain.RunMeta{
		IdleWritingEnabled:   true,
		PeakAutoPauseEnabled: true,
		Control:              &domain.RunControl{PeakOverrideUntil: peak.Add(30 * time.Minute).Format(time.RFC3339)},
	}
	idlePermit := domain.ResumePermit{Generation: 1, Origin: domain.RunOriginIdleScheduler, Trigger: domain.ResumeTriggerAfterPeak}
	if (&Host{}).scheduledResumeAllowed(idle, idlePermit, peak) {
		t.Fatal("idle task must remain blocked during peak even when manual skip is active")
	}
	if !(&Host{}).scheduledResumeAllowed(idle, idlePermit, open) {
		t.Fatal("enabled idle task should resume in an open window")
	}
	idle.IdleWritingEnabled = false
	if (&Host{}).scheduledResumeAllowed(idle, idlePermit, open) {
		t.Fatal("disabled idle writing must revoke idle-origin auto resume")
	}
}

func TestPauseCategoryForKindFailsClosedForAutomaticResume(t *testing.T) {
	tests := []struct {
		kind string
		want domain.StopCategory
	}{
		{kind: "advance_gate", want: domain.StopCategoryReviewGate},
		{kind: "deadlock", want: domain.StopCategoryFailureBreaker},
		{kind: "worker_failure", want: domain.StopCategoryDecisionFailed},
		{kind: "plan_start", want: domain.StopCategoryDecisionFailed},
		{kind: "backup", want: domain.StopCategoryDeterministicErr},
		{kind: "engine", want: domain.StopCategoryStateError},
		{kind: "unexpected", want: domain.StopCategoryUnknown},
	}
	for _, tt := range tests {
		if got := pauseCategoryForKind(tt.kind); got != tt.want {
			t.Errorf("pauseCategoryForKind(%q)=%q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestCancelPeakPolicyPauseDoesNotCancelIdleWindowPause(t *testing.T) {
	manual := &engine{running: true, pause: &pauseRequest{category: domain.StopCategoryPeakPolicy}}
	if !manual.cancelPeakPolicyPause() || manual.pause != nil {
		t.Fatal("pending manual peak pause should be cancellable")
	}
	idle := &engine{running: true, pause: &pauseRequest{category: domain.StopCategoryIdleWindowEnd}}
	if idle.cancelPeakPolicyPause() || idle.pause == nil {
		t.Fatal("pending idle high-peak pause must not be cancellable by manual skip")
	}
}
