package headless

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── fakeEngine：engineSource / continueEngine 的测试假实现 ──────────────

type fakeEngine struct {
	dir        string
	events     chan host.Event
	stream     chan string
	done       chan struct{}
	outcome    host.InterventionOutcome
	waitErr    error
	blockCh    chan struct{} // 非 nil 时 ContinueAndWait 阻塞直到 Close
	closed     chan struct{}
	closeCount int
}

func newFakeEngine(dir string) *fakeEngine {
	return &fakeEngine{
		dir:    dir,
		events: make(chan host.Event, 16),
		stream: make(chan string, 16),
		done:   make(chan struct{}, 4),
		closed: make(chan struct{}),
	}
}

func (f *fakeEngine) Dir() string               { return f.dir }
func (f *fakeEngine) Events() <-chan host.Event { return f.events }
func (f *fakeEngine) Stream() <-chan string     { return f.stream }
func (f *fakeEngine) Done() <-chan struct{}     { return f.done }

func (f *fakeEngine) Close() {
	f.closeCount++
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

func (f *fakeEngine) ContinueAndWait(text string) (host.InterventionOutcome, error) {
	if f.blockCh != nil {
		select {
		case <-f.closed:
			return f.outcome, f.waitErr
		case <-f.blockCh:
		}
	}
	return f.outcome, f.waitErr
}

// finish 模拟引擎完成。生产 host 只关闭 done（events/stream 永不关闭），
// consume 的终止路径是 done 信号 → drainPending 排空已缓冲事件；因此这里
// 也只关闭 done，保持与生产一致的事件投递语义。
func (f *fakeEngine) finish() {
	close(f.done)
}

// initRecoveryState 初始化"正常恢复状态"的 store：Progress + RunMeta 均在位，
// 无 PendingRewrites / AdvanceHold / error 事件时恢复判定为完成。测试必须
// 先铺好状态再跑 consume/runContinue——恢复完整性校验（incompleteRecoveryErr）
// 是 fail-closed 的，缺文件会返回"状态读取失败"而非具体的恢复问题。
func initRecoveryState(t *testing.T, dir string) *store.Store {
	t.Helper()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.Init("", "", ""); err != nil {
		t.Fatal(err)
	}
	return st
}

// writeMetaFile 写入 meta/ 下的状态文件（构造损坏 JSON 场景）。
func writeMetaFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── ExitCodeError ─────────────────────────────────────────────────────

func TestExitCodeError_UnwrapAndCode(t *testing.T) {
	cause := errors.New("boom")
	e := &ExitCodeError{Code: 5, Err: cause}
	if !errors.Is(e, cause) {
		t.Fatal("errors.Is must unwrap to cause")
	}
	var ec *ExitCodeError
	if !errors.As(e, &ec) || ec.Code != 5 {
		t.Fatal("errors.As must recover code 5")
	}
	if !strings.Contains(e.Error(), "boom") {
		t.Fatalf("Error() = %q, want cause text", e.Error())
	}
	if plain := (&ExitCodeError{Code: 3}).Error(); plain != "exit code 3" {
		t.Fatalf("plain Error() = %q, want exit code 3", plain)
	}
}

// ── consume：错误统计跨 drain 合并（不丢弃 Done 前已累计的 error 事件） ──

func TestConsume_ErrorStatsMergedAcrossDrain(t *testing.T) {
	eng := newFakeEngine(t.TempDir())
	eng.events <- host.Event{Level: "error", Category: "TOOL", Summary: "e1"}
	eng.done <- struct{}{} // Done 信号 → 进入 drainPending
	eng.events <- host.Event{Level: "error", Category: "TOOL", Summary: "e2"}
	eng.events <- host.Event{Level: "error", Category: "TOOL", Summary: "e3"}
	eng.finish()

	var stdout, stderr strings.Builder
	stats, err := consume(eng, &stdout, &stderr, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("error events must produce ExitCodeError{5}, got %v", err)
	}
	if stats.errorEvents != 3 {
		t.Fatalf("errorEvents = %d, want 3（drain 阶段不得丢弃主循环已累计的计数）", stats.errorEvents)
	}
}

// ── consume：恢复未完成 → ExitCodeError{5} ────────────────────────────

func TestConsume_IncompleteRecoveryErrorEvents(t *testing.T) {
	dir := t.TempDir()
	initRecoveryState(t, dir)
	eng := newFakeEngine(dir)
	eng.events <- host.Event{Level: "error", Category: "TOOL", Summary: "boom"}
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("expected ExitCodeError{5} on error events, got %v", err)
	}
	if !strings.Contains(err.Error(), "error 事件") {
		t.Fatalf("error should mention error events: %v", err)
	}
}

func TestConsume_IncompleteRecoveryPendingRewrites(t *testing.T) {
	dir := t.TempDir()
	st := initRecoveryState(t, dir)
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(dir)
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("expected ExitCodeError{5} on undrained rewrites, got %v", err)
	}
	if !strings.Contains(err.Error(), "PendingRewrites") {
		t.Fatalf("error should mention PendingRewrites: %v", err)
	}
}

func TestConsume_IncompleteRecoveryAdvanceHold(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.Init("", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetAdvanceHold(domain.AdvanceHold{
		After:  domain.AdvanceHoldAfterRewritesDrained,
		Reason: "改完让我验收",
	}); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(dir)
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("expected ExitCodeError{5} on unconsumed hold, got %v", err)
	}
	if !strings.Contains(err.Error(), "AdvanceHold") {
		t.Fatalf("error should mention AdvanceHold: %v", err)
	}
}

func TestConsume_CompleteRecoveryReturnsNil(t *testing.T) {
	dir := t.TempDir()
	initRecoveryState(t, dir)
	eng := newFakeEngine(dir)
	eng.finish()

	stats, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	if err != nil {
		t.Fatalf("clean finish should return nil: %v", err)
	}
	if stats.errorEvents != 0 {
		t.Fatalf("errorEvents = %d, want 0", stats.errorEvents)
	}
}

// ── runContinue：退出码语义 ────────────────────────────────────────────

func TestRunContinue_PendingSteerRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetPendingSteer("继续写"); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(dir)

	err := runContinue(eng, &strings.Builder{}, &strings.Builder{}, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 4 {
		t.Fatalf("expected ExitCodeError{4} for pending steer, got %v", err)
	}
}

func TestRunContinue_InterventionFailed(t *testing.T) {
	eng := newFakeEngine(t.TempDir())
	eng.outcome = host.InterventionOutcome{OK: false, Failure: errors.New("arbiter declined")}

	err := runContinue(eng, &strings.Builder{}, &strings.Builder{}, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 2 {
		t.Fatalf("expected ExitCodeError{2} for failed intervention, got %v", err)
	}
}

func TestRunContinue_EngineNotStarted(t *testing.T) {
	eng := newFakeEngine(t.TempDir())
	eng.outcome = host.InterventionOutcome{OK: true, EngineRunning: false, Failure: errors.New("engine busy")}

	err := runContinue(eng, &strings.Builder{}, &strings.Builder{}, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("expected ExitCodeError{3} for engine not started, got %v", err)
	}
}

func TestRunContinue_ContinueAndWaitError(t *testing.T) {
	eng := newFakeEngine(t.TempDir())
	eng.waitErr = errors.New("migration required")

	err := runContinue(eng, &strings.Builder{}, &strings.Builder{}, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("expected ExitCodeError{3} for ContinueAndWait error, got %v", err)
	}
}

func TestRunContinue_StartTimeout(t *testing.T) {
	old := continueStartTimeout
	continueStartTimeout = 50 * time.Millisecond
	defer func() { continueStartTimeout = old }()

	eng := newFakeEngine(t.TempDir())
	eng.blockCh = make(chan struct{}) // ContinueAndWait 阻塞 → 触发墙钟超时

	err := runContinue(eng, &strings.Builder{}, &strings.Builder{}, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("expected ExitCodeError{3} on start timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("error should mention timeout: %v", err)
	}
	if eng.closeCount == 0 {
		t.Fatal("Close must be called to abort the blocked intervention on timeout")
	}
}

func TestRunContinue_IncompleteRecoveryFails(t *testing.T) {
	dir := t.TempDir()
	st := initRecoveryState(t, dir)
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(dir)
	eng.outcome = host.InterventionOutcome{OK: true, EngineRunning: true}
	eng.events <- host.Event{Level: "error", Category: "TOOL", Summary: "boom"}
	eng.finish()

	var stderr strings.Builder
	err := runContinue(eng, &strings.Builder{}, &stderr, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("expected ExitCodeError{5} for incomplete recovery, got %v", err)
	}
	// 退出摘要必须先打印再上抛，诊断不丢失
	if !strings.Contains(stderr.String(), "退出摘要") {
		t.Fatalf("summary must be printed before error: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "error 事件: 1") {
		t.Fatalf("summary should report the error event: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "PendingRewrites: 未排空") {
		t.Fatalf("summary should report undrained rewrites: %q", stderr.String())
	}
}

func TestRunContinue_CompleteRecoverySucceeds(t *testing.T) {
	dir := t.TempDir()
	initRecoveryState(t, dir)
	eng := newFakeEngine(dir)
	eng.outcome = host.InterventionOutcome{OK: true, EngineRunning: true}
	eng.finish()

	var stderr strings.Builder
	if err := runContinue(eng, &strings.Builder{}, &stderr, "继续"); err != nil {
		t.Fatalf("clean continue should succeed: %v", err)
	}
	if !strings.Contains(stderr.String(), "退出摘要") {
		t.Fatalf("summary should be printed: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "error 事件: 0") {
		t.Fatalf("summary should report zero error events: %q", stderr.String())
	}
}

// ── 恢复状态读取失败（fail-closed）：缺失 / 损坏的状态文件 → 退出码 5 ──
//
// 阻断 2：恢复完整性校验无法核验状态就不能宣称成功——meta/progress.json 或
// meta/run.json 缺失 / 损坏均视为恢复未完成（ExitCodeError{5}），摘要打印
// "状态读取失败"，绝不误报"已排空/无"。

func TestConsume_MissingProgressFileFailsClosed(t *testing.T) {
	eng := newFakeEngine(t.TempDir()) // 空目录：meta/progress.json 缺失
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("missing progress.json must be ExitCodeError{5}, got %v", err)
	}
	if !strings.Contains(err.Error(), "状态读取失败") || !strings.Contains(err.Error(), "progress.json") {
		t.Fatalf("error should mention 状态读取失败 + progress.json: %v", err)
	}
}

func TestConsume_CorruptedProgressFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	writeMetaFile(t, dir, "meta/progress.json", []byte("{broken json"))
	eng := newFakeEngine(dir)
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("corrupted progress.json must be ExitCodeError{5}, got %v", err)
	}
	if !strings.Contains(err.Error(), "状态读取失败") {
		t.Fatalf("error should mention 状态读取失败: %v", err)
	}
}

func TestConsume_MissingRunMetaFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	// 不 Init RunMeta → meta/run.json 缺失
	eng := newFakeEngine(dir)
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("missing run.json must be ExitCodeError{5}, got %v", err)
	}
	if !strings.Contains(err.Error(), "状态读取失败") || !strings.Contains(err.Error(), "run.json") {
		t.Fatalf("error should mention 状态读取失败 + run.json: %v", err)
	}
}

func TestConsume_CorruptedRunMetaFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	writeMetaFile(t, dir, "meta/run.json", []byte("{broken json"))
	eng := newFakeEngine(dir)
	eng.finish()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("corrupted run.json must be ExitCodeError{5}, got %v", err)
	}
	if !strings.Contains(err.Error(), "状态读取失败") || !strings.Contains(err.Error(), "run.json") {
		t.Fatalf("error should mention 状态读取失败 + run.json: %v", err)
	}
}

func TestRunContinue_MissingProgressFailsClosed(t *testing.T) {
	eng := newFakeEngine(t.TempDir()) // 空目录：meta/progress.json 缺失
	eng.outcome = host.InterventionOutcome{OK: true, EngineRunning: true}
	eng.finish()

	var stderr strings.Builder
	err := runContinue(eng, &strings.Builder{}, &stderr, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("missing progress.json must be ExitCodeError{5}, got %v", err)
	}
	if !strings.Contains(err.Error(), "状态读取失败") {
		t.Fatalf("error should mention 状态读取失败: %v", err)
	}
	// 退出摘要先打印再上抛：诊断信息不丢失，且明确打印"状态读取失败"
	if !strings.Contains(stderr.String(), "状态读取失败") {
		t.Fatalf("summary should print 状态读取失败: %q", stderr.String())
	}
}

func TestRunContinue_CorruptedProgressFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	writeMetaFile(t, dir, "meta/progress.json", []byte("{broken json"))
	eng := newFakeEngine(dir)
	eng.outcome = host.InterventionOutcome{OK: true, EngineRunning: true}
	eng.finish()

	var stderr strings.Builder
	err := runContinue(eng, &strings.Builder{}, &stderr, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("corrupted progress.json must be ExitCodeError{5}, got %v", err)
	}
	if !strings.Contains(err.Error(), "状态读取失败") {
		t.Fatalf("error should mention 状态读取失败: %v", err)
	}
	if !strings.Contains(stderr.String(), "状态读取失败") {
		t.Fatalf("summary should print 状态读取失败: %q", stderr.String())
	}
}
