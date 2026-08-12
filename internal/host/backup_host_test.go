package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	corecontext "github.com/voocel/agentcore/context"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/agents/ctxpack"
	"github.com/voocel/ainovel-cli/internal/backup"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// ── helpers ──

func oe(ch int) domain.OutlineEntry {
	return domain.OutlineEntry{Chapter: ch, Title: fmt.Sprintf("Ch%d", ch), CoreEvent: "ev"}
}

func writeLayeredOutline(t *testing.T, st *storepkg.Store, vols []domain.VolumeOutline) {
	t.Helper()
	if err := st.Outline.SaveLayeredOutline(vols); err != nil {
		t.Fatal(err)
	}
}

func writeProgress(t *testing.T, st *storepkg.Store, p *domain.Progress) {
	t.Helper()
	if err := st.Progress.Save(p); err != nil {
		t.Fatal(err)
	}
}

func initTestProject(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":1}`), 0o644)
	os.MkdirAll(filepath.Join(src, "chapters"), 0o755)
	os.WriteFile(filepath.Join(src, "chapters", "01.md"), []byte("# Ch1\ncontent."), 0o644)
	os.MkdirAll(filepath.Join(src, "meta"), 0o755)
	os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(src, "meta", "progress.json"), []byte(`{"phase":"writing"}`), 0o644)
	return src
}

func newTestHost(t *testing.T, src string) *Host {
	t.Helper()
	st := storepkg.NewStore(src)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	usage := NewUsageTracker(nil, st)
	uc, ucancel := context.WithCancel(context.Background())
	usage.StartAutoSave(uc)
	e := &engine{
		store:     st,
		notify:    func(_, _, _, _ string) {},
		emitEvent: func(Event) {},
		onPause:   func(string) {},
		onDone:    func() {},
	}
	e.gate = NewChapterAdvanceGate(st, func(string) {}, func(string, string) {})
	h := &Host{
		store:         st,
		engine:        e,
		usage:         usage,
		usageCtx:      uc,
		usageCancel:   ucancel,
		events:        make(chan Event, 100),
		streamCh:      make(chan string, 256),
		done:          make(chan struct{}, 4),
		mu:            sync.Mutex{},
		interMu:       sync.Mutex{},
		writerRestore: &ctxpack.WriterRestorePack{},
	}
	h.observer = newObserver(st, h.emitEvent, h.emitDelta, h.emitClear)
	return h
}

// ── 1. Corrupt Progress + pending instruction: worker never invoked ──

func TestEngine_CorruptProgressNoWorker(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(st.Dir(), "meta", "progress.json"), []byte("{corrupt"), 0o644)

	var workerCalls int32
	e := &engine{
		store:           st,
		workers:         subagent.NewRunner(),
		notify:          func(_, _, _, _ string) {},
		emitEvent:       func(Event) {},
		onPause:         func(string) {},
		onDone:          func() {},
		beforeRunWorker: func() { atomic.AddInt32(&workerCalls, 1) },
	}
	e.gate = NewChapterAdvanceGate(st, func(string) {}, func(string, string) {})
	e.next = &flow.Instruction{Agent: "writer", Task: "write pending chapter"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.done = done
	e.running = true
	e.cancel = cancel
	go e.run(ctx)
	<-done

	if n := atomic.LoadInt32(&workerCalls); n != 0 {
		t.Fatalf("runWorker called %d times, expected 0", n)
	}
}

// ── 2. Worker boundary: exactly one snapshot via real runWorker ──

func TestEngine_WorkerBoundaryOneSnapshot(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	writeLayeredOutline(t, st, []domain.VolumeOutline{{
		Index: 1, Title: "V1",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "A1", Chapters: []domain.OutlineEntry{oe(1), oe(2)}},
			{Index: 2, Title: "A2", Chapters: []domain.OutlineEntry{oe(3), oe(4)}},
		},
	}})
	// Chapter 1 completed (before=1), phase=writing, TotalChapters=2 so engine completes after ch2
	writeProgress(t, st, &domain.Progress{
		CurrentVolume: 1, CurrentArc: 1,
		CompletedChapters: []int{1},
		Phase:             domain.PhaseWriting,
		TotalChapters:     2,
	})
	// Draft needed by commit_chapter tool
	if err := st.Drafts.SaveDraft(2, "# Ch2\nboundary test content. 她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}

	var snapCalls int32
	var volCalls int32

	// Scripted model calls commit_chapter on first turn
	model := &scriptedChatModel{fn: func(msgs []agentcore.Message) agentcore.Message {
		return testToolCallMsg("commit_chapter", map[string]any{
			"chapter":         2,
			"summary":         "Ch2 boundary",
			"characters":      []string{"主角"},
			"key_events":      []string{"推进"},
			"hook_type":       "crisis",
			"dominant_strand": "quest",
		})
	}}
	writer := subagent.Config{
		Name: "writer", Description: "boundary test writer",
		Model: model, SystemPrompt: "test", MaxTurns: 5,
		Tools:          []agentcore.Tool{tools.NewCommitChapterTool(st)},
		StopAfterTools: []string{"commit_chapter"},
	}

	e, _, done := newTestEngine(t, st, subagent.NewRunner(writer), nil)
	e.backupArc = func(v, a int) error {
		atomic.AddInt32(&snapCalls, 1)
		return nil
	}
	e.backupVolume = func(v int) error {
		atomic.AddInt32(&volCalls, 1)
		return nil
	}

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	if n := atomic.LoadInt32(&snapCalls); n != 1 {
		t.Fatalf("expected 1 arc snapshot, got %d", n)
	}
	if n := atomic.LoadInt32(&volCalls); n != 0 {
		t.Fatalf("expected 0 volume snapshots, got %d", n)
	}
	// Ensure progress advanced
	p, err := st.Progress.Load()
	if err != nil || p == nil {
		t.Fatal("progress must be loadable")
	}
	if len(p.CompletedChapters) != 2 {
		t.Fatalf("expected 2 completed chapters, got %v", p.CompletedChapters)
	}
}

// ── 3. onDone blocks: running true, restart rejected, wait blocks, release ──

func TestEngine_OnDoneBlocksRestart(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatal(err)
	}

	onDoneEntered := make(chan struct{})
	releaseOnDone := make(chan struct{})
	e := &engine{
		store:     st,
		workers:   subagent.NewRunner(),
		notify:    func(_, _, _, _ string) {},
		emitEvent: func(Event) {},
		onPause:   func(string) {},
		onDone: func() {
			close(onDoneEntered)
			<-releaseOnDone
		},
	}
	e.gate = NewChapterAdvanceGate(st, func(string) {}, func(string, string) {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.done = done
	e.running = true
	e.cancel = cancel
	go e.run(ctx)
	<-onDoneEntered

	if !e.isRunning() {
		t.Fatal("engine must report running=true while onDone executes")
	}
	if e.start(nil) {
		t.Fatal("restart must be rejected while engine completes")
	}
	close(releaseOnDone)
	<-done
	if e.isRunning() {
		t.Fatal("engine must report not running after done")
	}
}

// ── 4. Host restore lifecycle paused ──

func TestHostRestoreLifecyclePaused(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, h *Host) (snapID string, expectErr bool){
		"success": func(t *testing.T, h *Host) (string, bool) {
			m, err := backup.Backup(h.store.Dir(), "p", backup.KindVolume, 1, 0)
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			os.WriteFile(filepath.Join(h.store.Dir(), "outline.json"), []byte("modified"), 0o644)
			return m.SnapshotID, false
		},
		"partial": func(t *testing.T, h *Host) (string, bool) {
			m, err := backup.Backup(h.store.Dir(), "p", backup.KindVolume, 1, 0)
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			os.Remove(filepath.Join(h.store.Dir(), "outline.json"))
			os.MkdirAll(filepath.Join(h.store.Dir(), "outline.json"), 0o755)
			return m.SnapshotID, true
		},
		"missing": func(t *testing.T, h *Host) (string, bool) {
			return "nonexistent", true
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":3}`), 0o644)
			os.MkdirAll(filepath.Join(src, "chapters"), 0o755)
			os.WriteFile(filepath.Join(src, "chapters", "01.md"), []byte("# Ch1\ncontent."), 0o644)
			os.MkdirAll(filepath.Join(src, "meta"), 0o755)
			os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)
			os.WriteFile(filepath.Join(src, "meta", "progress.json"), []byte(`{"phase":"writing"}`), 0o644)

			h := newTestHost(t, src)
			// No manual lifecycle manipulation — RestoreSnapshot owns lifecycle transitions
			snapID, expectErr := setup(t, h)

			rr, err := h.RestoreSnapshot(snapID, true)
			if expectErr && err == nil {
				t.Fatal("expected error")
			}
			if !expectErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h.lifecycle != lifecyclePaused {
				t.Fatalf("expected lifecycle paused, got %s", h.lifecycle)
			}
			if rr != nil && !expectErr {
				if !rr.FinalVerify || rr.Failed != 0 {
					t.Fatal("full success expected")
				}
				data, _ := os.ReadFile(filepath.Join(src, "outline.json"))
				if strings.TrimSpace(string(data)) != `{"chapters":3}` {
					t.Fatal("content not reverted")
				}
				if _, verr := backup.Verify(rr.RescuePath); verr != nil {
					t.Fatalf("rescue verify: %v", verr)
				}
			}
		})
	}
}

// ── 5. WRP Hook restored content via refresh ──

func TestHostRestoreWRPHook(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "chapters"), 0o755)
	os.WriteFile(filepath.Join(src, "chapters", "01.md"), []byte("# Ch1\noriginal."), 0o644)
	os.WriteFile(filepath.Join(src, "chapters", "02.md"), []byte("# Ch2\ncontent."), 0o644)
	os.MkdirAll(filepath.Join(src, "meta"), 0o755)
	os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)

	// Seed store with enough content for WriterRestorePack.Refresh to produce data.
	st := storepkg.NewStore(src)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	// Progress with CurrentChapter=2 so loadWriterRestoreState has a target.
	writeProgress(t, st, &domain.Progress{
		CurrentChapter: 2,
		TotalChapters:  3,
		Phase:          domain.PhaseWriting,
	})
	// Outline entry for chapter 2 so GetChapterOutline succeeds.
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "Ch1", CoreEvent: "start"},
		{Chapter: 2, Title: "Ch2", CoreEvent: "middle"},
	}); err != nil {
		t.Fatal(err)
	}
	// Chapter plan so loadWriterRestoreState has non-empty content.
	if err := st.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 2,
		Goal:    "advance the plot through the middle act",
	}); err != nil {
		t.Fatal(err)
	}

	// Backup and then corrupt the active tree.
	m, err := backup.Backup(src, "p", backup.KindVolume, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Replace outline.json with garbage to prove restore reverts it.
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`"modified"`), 0o644)

	// 复核阻塞项 2 单写者语义：newTestHost 会再建一个 Store，先释放种子 store 的锁。
	st.Close()
	h := newTestHost(t, src)
	rr, rerr := h.RestoreSnapshot(m.SnapshotID, true)
	if rerr != nil {
		t.Fatalf("Restore: %v", rerr)
	}
	if !rr.FinalVerify {
		t.Fatal("final verify must pass")
	}

	// refreshWriterRestore was called during RestoreSnapshot — invoke Hook
	// to obtain the actual restored messages.
	hook := h.writerRestore.Hook()
	msgs, hookErr := hook(context.Background(), corecontext.SummaryInfo{}, nil)
	if hookErr != nil {
		t.Fatalf("WRP hook: %v", hookErr)
	}
	if len(msgs) == 0 {
		t.Fatal("WRP Hook must return at least one restored-content message")
	}
	// The restored content must reference the snapshot's chapter 2 plan.
	text := msgs[0].TextContent()
	if !strings.Contains(text, "advance the plot") || !strings.Contains(text, "middle act") {
		t.Fatalf("restored WRP content must contain snapshot chapter plan, got: %s", text)
	}
}

// ── 6. Usage nonzero persists/reloads/autosave via actual SaveNow+Load ──

func TestUsagePersistsAutosave(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{}`), 0o644)
	os.MkdirAll(filepath.Join(src, "meta"), 0o755)
	os.WriteFile(filepath.Join(src, "meta", "progress.json"), []byte(`{"phase":"writing"}`), 0o644)
	os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)

	m, _ := backup.Backup(src, "p", backup.KindVolume, 1, 0)
	h := newTestHost(t, src)
	// Seed nonzero usage via real Record path, then SaveNow persists to disk.
	h.usage.Record("writer", "", makeUsageMsg(100, 0, 0, 50))
	h.usage.SaveNow()

	// Restore — this stops autosave, persists pre-restore usage, restores files,
	// persists post-restore usage, and restarts autosave.
	_, err := h.RestoreSnapshot(m.SnapshotID, true)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Confirm pre-restore seeded usage survived on disk.
	fresh := NewUsageTracker(nil, h.store)
	loaded, err := fresh.LoadFromStore()
	if err != nil || !loaded {
		t.Fatal("fresh tracker must load persisted usage from disk")
	}
	_, in, out, _, _ := fresh.Totals()
	if in < 100 || out < 50 {
		t.Fatalf("fresh tracker must see seeded usage (in=%d out=%d)", in, out)
	}

	// Record post-restore usage via the Host tracker (h.usage has a live
	// autoSaveLoop). Do NOT call SaveNow manually — autosave must persist.
	h.usage.Record("editor", "", makeUsageMsg(50, 0, 0, 25))

	// Poll with a fresh tracker bounded by a deadline until autosave writes
	// the cumulative (100+50, 50+25) totals to disk.
	const pollInterval = 50 * time.Millisecond
	deadline := time.After(3 * time.Second)
	var pollIn, pollOut int
	for {
		poll := NewUsageTracker(nil, h.store)
		if ok, _ := poll.LoadFromStore(); ok {
			_, pollIn, pollOut, _, _ = poll.Totals()
			if pollIn >= 150 && pollOut >= 75 {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("autosave did not persist cumulative totals within deadline (in=%d out=%d)", pollIn, pollOut)
		case <-time.After(pollInterval):
		}
	}
}

// ── 7. Backup mid-copy blocks start (channel-await, no fixed sleep) ──

func TestBackupMidCopyBlocksStart(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	backup.SetBackupHooks(backup.BackupHooks{
		BeforeWalkCopy: func() error {
			close(blocked)
			<-release
			return nil
		},
	})
	defer backup.SetBackupHooks(backup.BackupHooks{})

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":2}`), 0o644)
	os.MkdirAll(filepath.Join(src, "chapters"), 0o755)
	os.WriteFile(filepath.Join(src, "chapters", "01.md"), []byte("# Ch1\ncontent."), 0o644)
	os.WriteFile(filepath.Join(src, "chapters", "02.md"), []byte("# Ch2\ncontent."), 0o644)
	os.MkdirAll(filepath.Join(src, "meta"), 0o755)
	os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(src, "meta", "progress.json"), []byte(`{"phase":"writing"}`), 0o644)

	st := storepkg.NewStore(src)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	writeLayeredOutline(t, st, []domain.VolumeOutline{{
		Index: 1, Title: "V1",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "A1", Chapters: []domain.OutlineEntry{oe(1), oe(2)}},
			{Index: 2, Title: "A2", Chapters: []domain.OutlineEntry{oe(3), oe(4)}},
		},
	}})
	writeProgress(t, st, &domain.Progress{CurrentVolume: 1, CurrentArc: 1, CompletedChapters: []int{2}})
	// 复核阻塞项 2 单写者语义：newTestHost 会再建一个 Store，先释放种子 store 的锁。
	st.Close()
	h := newTestHost(t, src)

	backupErr := make(chan error, 1)
	go func() {
		_, err := h.BackupArc(1, 1)
		backupErr <- err
	}()
	<-blocked

	// startEngine must block (interMu held by BackupArc)
	startStarted := make(chan struct{})
	go func() {
		h.startEngine(nil)
		close(startStarted)
	}()

	// Non-blocking check: startEngine must NOT have completed yet
	select {
	case <-startStarted:
		t.Fatal("startEngine must block during backup")
	default:
	}

	close(release)
	err := <-backupErr
	if err != nil {
		t.Fatalf("backup failed: %v", err)
	}

	// After backup releases, startEngine should complete
	select {
	case <-startStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("startEngine did not proceed after backup")
	}
}

// ── 8. Partial restore zero writes, no rescue ──

func TestPartialRestoreZeroWritesNoRescue(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":3}`), 0o644)
	os.MkdirAll(filepath.Join(src, "chapters"), 0o755)
	os.WriteFile(filepath.Join(src, "chapters", "01.md"), []byte("# Ch1\ncontent."), 0o644)
	os.MkdirAll(filepath.Join(src, "meta"), 0o755)
	os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(src, "meta", "progress.json"), []byte(`{"phase":"writing"}`), 0o644)

	m, _ := backup.Backup(src, "p", backup.KindVolume, 1, 0)
	os.Remove(filepath.Join(src, "outline.json"))
	os.MkdirAll(filepath.Join(src, "outline.json"), 0o755)

	h := newTestHost(t, src)
	rr, err := h.RestoreSnapshot(m.SnapshotID, true)
	if err == nil {
		t.Fatal("expected error for partial restore")
	}
	if rr.Succeeded != 0 {
		t.Fatalf("zero writes expected, got %d", rr.Succeeded)
	}
	if rr.Failed == 0 {
		t.Fatal("expected failures")
	}
	if rr.RescueID != "" {
		t.Fatal("no rescue on preflight failure")
	}
	if rr.RescuePath != "" {
		t.Fatal("no rescue path on preflight failure")
	}
}

// ── 9. startEngine reserved/unreserved paths: no deadlock ──

func TestStartEngineNoDeadlock(t *testing.T) {
	t.Run("unreserved", func(t *testing.T) {
		src := initTestProject(t)
		h := newTestHost(t, src)

		// Unreserved path: startEngine wrapper (acquires interMu internally)
		started := make(chan bool)
		go func() {
			started <- h.startEngine(nil)
		}()
		select {
		case ok := <-started:
			if !ok {
				t.Fatal("startEngine should return true for successful start")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("startEngine wrapper deadlocked")
		}
	})

	t.Run("reserved", func(t *testing.T) {
		src := initTestProject(t)
		h := newTestHost(t, src)

		// Reserved path: startEngineLocked under interMu (like StartPrepared)
		started := make(chan bool)
		go func() {
			h.interMu.Lock()
			defer h.interMu.Unlock()
			started <- h.startEngineLocked(nil)
		}()
		select {
		case ok := <-started:
			if !ok {
				t.Fatal("startEngineLocked should return true for successful start")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("startEngineLocked deadlocked under prior interMu")
		}
	})
}

// ── 10. Real Continue/doIntervention with hook blocks RestoreSnapshot ──

func TestContinueInterventionBlocksRestore(t *testing.T) {
	src := initTestProject(t)
	h := newTestHost(t, src)

	// Hook doIntervention to block inside interMu (before arbiter)
	hookEntered := make(chan struct{})
	releaseHook := make(chan struct{})
	testBeforeArbiter = func() error {
		close(hookEntered)
		<-releaseHook
		return fmt.Errorf("hook abort")
	}
	defer func() { testBeforeArbiter = nil }()

	// Start Continue — this triggers doIntervention in a goroutine
	// doIntervention acquires interMu, then hits the hook and blocks
	continueDone := make(chan struct{})
	go func() {
		h.Continue("keep writing")
		close(continueDone)
	}()

	<-hookEntered // doIntervention is blocking inside interMu

	// RestoreSnapshot should block on interMu
	restoreDone := make(chan error, 1)
	go func() {
		_, err := h.RestoreSnapshot("any-snap", true)
		restoreDone <- err
	}()

	// Non-blocking check: RestoreSnapshot must NOT have completed
	select {
	case <-restoreDone:
		t.Fatal("restore must block while intervention holds interMu")
	default:
	}

	close(releaseHook)

	// Intervention completes
	select {
	case <-continueDone:
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not complete after hook release")
	}

	// After intervention releases interMu, restore should complete
	select {
	case <-restoreDone:
	case <-time.After(5 * time.Second):
		t.Fatal("restore did not complete after intervention release")
	}
}

// ── 11. Real ImportFrom contention with RestoreSnapshot ──

func TestImportFromBlocksRestore(t *testing.T) {
	src := initTestProject(t)
	h := newTestHost(t, src)

	// Hook async ops to block before reading the operation channel
	hookEntered := make(chan struct{})
	releaseHook := make(chan struct{})
	testBeforeAsyncOp = func() error {
		close(hookEntered)
		<-releaseHook
		return nil
	}
	defer func() { testBeforeAsyncOp = nil }()

	// Hook ImportFrom to skip model-dependent imp.Run, returning a controlled channel
	ctx := context.Background()
	importCh := make(chan imp.Event, 1)
	testBeforeImpRun = func(_ context.Context) (<-chan imp.Event, error) {
		return importCh, nil
	}
	defer func() { testBeforeImpRun = nil }()

	// Real ImportFrom — exercises guardExclusive, activeOps.Add(1), trackImpOp
	importDone := make(chan struct{})
	go func() {
		_, err := h.ImportFrom(ctx, imp.Options{})
		if err != nil {
			t.Logf("ImportFrom returned: %v", err)
		}
		close(importDone)
	}()
	<-hookEntered // async op is holding activeOps

	// RestoreSnapshot should block on activeOps.Wait()
	restoreDone := make(chan error, 1)
	go func() {
		_, err := h.RestoreSnapshot("any-snap", true)
		restoreDone <- err
	}()

	// Non-blocking check: restore must NOT have completed
	select {
	case <-restoreDone:
		t.Fatal("restore must block while async op active")
	default:
	}

	close(releaseHook)
	close(importCh) // unblock for-range loop in trackImpOp

	select {
	case <-importDone:
	case <-time.After(5 * time.Second):
		t.Fatal("async op did not complete after hook release")
	}

	// After async op releases, restore should complete
	select {
	case <-restoreDone:
	case <-time.After(5 * time.Second):
		t.Fatal("restore did not complete after async op release")
	}
}

// ── 12. Confirmation required ──

func TestRestoreRequiresConfirmation(t *testing.T) {
	h := newTestHost(t, initTestProject(t))
	_, err := h.RestoreSnapshot("x", false)
	if err == nil {
		t.Fatal("unconfirmed must be rejected")
	}
}

// ── 13. List excludes rescues ──

func TestHostListExcludesRescues(t *testing.T) {
	src := initTestProject(t)
	m, _ := backup.Backup(src, "p", backup.KindVolume, 1, 0)
	root, _ := backup.BackupRoot(src)
	os.MkdirAll(filepath.Join(root, ".rescue", "r1", "data"), 0o755)
	os.WriteFile(filepath.Join(root, ".rescue", "r1", "COMPLETE"), []byte("r1"), 0o644)

	h := newTestHost(t, src)
	snaps, _ := h.ListSnapshots()
	for _, s := range snaps {
		if s.SnapshotID == "r1" {
			t.Fatal("rescue in list")
		}
	}
	found := false
	for _, s := range snaps {
		if s.SnapshotID == m.SnapshotID {
			found = true
		}
	}
	if !found {
		t.Fatal("normal backup missing")
	}
}

// ── 14. Boundary valid/mismatched ──

func TestBackupBoundaryValidMismatched(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":2}`), 0o644)
	os.MkdirAll(filepath.Join(src, "chapters"), 0o755)
	os.WriteFile(filepath.Join(src, "chapters", "01.md"), []byte("# Ch1\ncontent."), 0o644)
	os.WriteFile(filepath.Join(src, "chapters", "02.md"), []byte("# Ch2\ncontent."), 0o644)
	os.MkdirAll(filepath.Join(src, "meta"), 0o755)
	os.WriteFile(filepath.Join(src, "meta", "run.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(src, "meta", "progress.json"), []byte(`{"phase":"writing"}`), 0o644)

	st := storepkg.NewStore(src)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	writeLayeredOutline(t, st, []domain.VolumeOutline{{
		Index: 1, Title: "V1",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "A1", Chapters: []domain.OutlineEntry{oe(1), oe(2)}},
			{Index: 2, Title: "A2", Chapters: []domain.OutlineEntry{oe(3), oe(4)}},
		},
	}})
	writeProgress(t, st, &domain.Progress{CurrentVolume: 1, CurrentArc: 1, CompletedChapters: []int{2}})
	// 复核阻塞项 2（方案 A）：同一 workspace 只允许一个可写 Store——铺完种子
	// 状态后释放，newTestHost 内部 store 才能取锁（种子已持久化在磁盘）。
	st.Close()
	h := newTestHost(t, src)

	m, err := h.BackupArc(1, 1)
	if err != nil {
		t.Fatalf("valid arc: %v", err)
	}
	if m.Kind != backup.KindArc || m.Volume != 1 || m.Arc != 1 {
		t.Fatal("unexpected manifest")
	}
	_, err = h.BackupArc(1, 99)
	if err == nil {
		t.Fatal("mismatched arc should fail")
	}
	_, err = h.BackupVolume(1)
	if err == nil || !strings.Contains(err.Error(), "volume end") {
		t.Fatalf("non-volume-end should fail: %v", err)
	}
}

// ── 15. Host.Close 释放 workspace 排他锁（复核阻塞项 3）──

// TestHostClose_ReleasesWorkspaceLock 验证 Host.Close 释放 workspace 排他锁：
// 关闭后同一进程可重新 NewStore 同一 workspace（旧实现锁随进程存活，同目录
// 第二个可写 Store 会被单写者规则拒绝）。
func TestHostClose_ReleasesWorkspaceLock(t *testing.T) {
	src := t.TempDir()
	h := newTestHost(t, src)
	h.Close()

	st2 := storepkg.NewStore(src)
	if !st2.Ready() {
		t.Fatalf("store after host close must be ready, init err=%v", st2.Init())
	}
	st2.Close()
}
