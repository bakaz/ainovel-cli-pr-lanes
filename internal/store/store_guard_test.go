package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ── 缺口 1：checkpoint 损坏的 Store 写操作 fail-closed ──────────────────

// TestStoreGuard_CheckpointCorruptWritesBlocked 覆盖缺口 1：checkpoint 数据损坏
// 的 Store 不仅 Ready()/Init() 报错——所有子 Store 的写操作也必须经统一 guard
// 拒绝（旧实现只设 initErr，Drafts.SaveDraft 等仍可写盘）。
func TestStoreGuard_CheckpointCorruptWritesBlocked(t *testing.T) {
	dir := t.TempDir()
	// 手工构造重复 seq 的损坏 checkpoint 文件。
	jsonlPath := filepath.Join(dir, checkpointsFile)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	data := "{\"seq\":1,\"scope\":{\"kind\":\"chapter\",\"chapter\":1},\"step\":\"plan\",\"occurred_at\":\"2026-01-01T00:00:00Z\"}\n" +
		"{\"seq\":1,\"scope\":{\"kind\":\"chapter\",\"chapter\":1},\"step\":\"polish\",\"occurred_at\":\"2026-01-01T00:00:01Z\"}\n"
	if err := os.WriteFile(jsonlPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	st := NewStore(dir)
	if st.Ready() {
		t.Fatal("corrupt-checkpoint store must not be ready")
	}
	if err := st.Init(); err == nil || !strings.Contains(err.Error(), "数据损坏") {
		t.Fatalf("Init must surface checkpoint corruption, got: %v", err)
	}

	// 缺口 1：所有子 Store 写操作必须 fail-closed（统一 guard，而非只 Init 报错）。
	if err := st.Drafts.SaveDraft(1, "x"); err == nil {
		t.Fatal("Drafts.SaveDraft must fail on corrupt store")
	}
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "plan", "a", "sha256:1"); err == nil {
		t.Fatal("Checkpoints.Append must fail on corrupt store")
	}
	if err := st.Progress.Save(&domain.Progress{}); err == nil {
		t.Fatal("Progress.Save must fail on corrupt store")
	}
	if err := st.Outline.ClearOutlineFeedback(); err == nil {
		t.Fatal("ClearOutlineFeedback must fail on corrupt store")
	}
	if err := st.Runtime.Reset(); err == nil {
		t.Fatal("Runtime.Reset must fail on corrupt store")
	}
	if err := st.StyleReview.Save(domain.StyleReviewLedger{SchemaVersion: 1, Chapter: 1}); err == nil {
		t.Fatal("StyleReview.Save must fail on corrupt store")
	}

	// 磁盘未被修改（guard 在写盘前拦截）。
	if _, err := os.Stat(filepath.Join(dir, "drafts", "01.draft.md")); !os.IsNotExist(err) {
		t.Fatal("corrupt store must not create draft file")
	}
}

// ── 缺口 2：Close 后 Store 写操作 fail-closed ──────────────────────────

// TestStoreGuard_ClosedWritesBlocked 覆盖缺口 2：Close() 后写操作返回稳定
// "store is closed" 错误、Ready()=false、Init() 报错；且 stA.Close() → stB 重新
// 获取锁后，stA 仍绝不能写盘（杜绝双写者）。
func TestStoreGuard_ClosedWritesBlocked(t *testing.T) {
	dir := t.TempDir()
	stA := NewStore(dir)
	if err := stA.Init(); err != nil {
		t.Fatal(err)
	}
	if err := stA.Drafts.SaveDraft(1, "正文"); err != nil {
		t.Fatal(err)
	}

	stA.Close()
	if stA.Ready() {
		t.Fatal("closed store must not be ready")
	}
	if err := stA.Init(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store Init must fail with stable closed error, got: %v", err)
	}
	if err := stA.Drafts.SaveDraft(1, "覆盖"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store write must fail with stable closed error, got: %v", err)
	}
	if _, err := stA.Checkpoints.Append(domain.ChapterScope(1), "plan", "a", "sha256:1"); err == nil {
		t.Fatal("closed store checkpoint append must fail")
	}
	if err := stA.Progress.Save(&domain.Progress{}); err == nil {
		t.Fatal("closed store progress save must fail")
	}

	// stA 关闭后 stB 重新获取锁（单写者槽位已释放）——stA 绝不能继续写（双写者）。
	stB := NewStore(dir)
	if !stB.Ready() {
		t.Fatalf("store B must acquire the workspace after A closed: %v", stB.Init())
	}
	if err := stA.Drafts.SaveDraft(1, "A 再写"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store A must not write while B holds the lock, got: %v", err)
	}
	if err := stB.Drafts.SaveDraft(1, "B 写入"); err != nil {
		t.Fatalf("store B write failed: %v", err)
	}
	stB.Close()

	// Close 幂等 + 并发安全（连续多次调用不 panic、不重复释放）。
	stA.Close()
	stA.Close()
}

// ── 缺口 3：直接 OS 写操作绕过守卫 ─────────────────────────────────────

// TestStoreGuard_ReadOnlyNoWorkspaceMutation 覆盖缺口 3：ClearOutlineFeedback /
// Runtime.Reset 在只读 Store 上经统一 guard 拒绝、不修改 workspace；Close 后
// 同样拒绝。
func TestStoreGuard_ReadOnlyNoWorkspaceMutation(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.AppendOutlineFeedback(ChapterFeedback{Chapter: 3, Deviation: "支线膨胀", Suggestion: "收线"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Runtime.AppendQueue(domain.RuntimeQueueItem{
		Kind: domain.RuntimeQueueUIEvent, Priority: domain.RuntimePriorityBackground, Summary: "s",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	stR := NewReadOnlyStore(dir)
	// 只读：写操作拒绝 + workspace 不变。
	if err := stR.Outline.ClearOutlineFeedback(); err == nil {
		t.Fatal("read-only ClearOutlineFeedback must fail")
	}
	if err := stR.Runtime.Reset(); err == nil {
		t.Fatal("read-only Runtime.Reset must fail")
	}
	if data, err := os.ReadFile(filepath.Join(dir, outlineFeedbackFile)); err != nil || len(data) == 0 {
		t.Fatalf("feedback file must remain on read-only store, err=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, runtimeQueuePath)); err != nil || len(data) == 0 {
		t.Fatalf("runtime queue must remain on read-only store, err=%v", err)
	}
	stR.Close()

	// Close 后同样拒绝（经统一 guard 的 errStoreClosed）。
	if err := st.Outline.ClearOutlineFeedback(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store ClearOutlineFeedback must fail with closed error, got: %v", err)
	}
	if err := st.Runtime.Reset(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store Runtime.Reset must fail with closed error, got: %v", err)
	}
	// 可写 Store 重新打开后仍可正常清空（guard 未误伤合法路径）。
	st2 := NewStore(dir)
	if err := st2.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st2.Outline.ClearOutlineFeedback(); err != nil {
		t.Fatalf("writable store ClearOutlineFeedback must work: %v", err)
	}
	if err := st2.Runtime.Reset(); err != nil {
		t.Fatalf("writable store Runtime.Reset must work: %v", err)
	}
	st2.Close()
}

// ── 缺口：Close 与在途写竞争窗口（写生命周期 lease）─────────────────────

// TestStoreClose_WaitsForInFlightWrite 覆盖缺口：物理写在途（已持写生命周期读
// lease）时 Close() 必须阻塞等待其完成，之后才释放 workspace 锁——观测点：
//   - 在途写期间 Close 不返回（独占 lease 被读 lease 阻塞）；
//   - 在途写期间新 Store 无法获取 workspace 锁（旧 Store 锁未释放）；
//   - 写完成后 Close 返回，新 Store 可拿锁；旧 Store 写被 closed guard 拒绝。
func TestStoreClose_WaitsForInFlightWrite(t *testing.T) {
	dir := t.TempDir()
	stA := NewStore(dir)
	if err := stA.Init(); err != nil {
		t.Fatal(err)
	}

	// 可观测慢写：物理写在获取 lease 并 guard 通过后、OS 修改前阻塞。
	entered := make(chan struct{})
	release := make(chan struct{})
	SetWriteHooks(WriteHooks{BeforePhysicalWrite: func() {
		close(entered)
		<-release
	}})
	defer SetWriteHooks(WriteHooks{})

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- stA.Drafts.SaveDraft(1, "慢写在途内容")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not enter physical phase")
	}

	closeDone := make(chan struct{})
	go func() {
		stA.Close()
		close(closeDone)
	}()
	// Close 必须等待在途写完成（独占 lease 被读 lease 阻塞）。
	select {
	case <-closeDone:
		t.Fatal("Close must wait for the in-flight write to finish")
	case <-time.After(150 * time.Millisecond):
	}

	// 在途写期间：workspace 锁仍未释放——新 Store 无法获取。
	stMid := NewStore(dir)
	if stMid.Ready() {
		t.Fatal("workspace lock must still be held while Close waits for the in-flight write")
	}
	stMid.Close()

	// 释放在途写 → 写完成 → Close 继续并释放 workspace 锁。
	close(release)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("in-flight write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight write did not complete")
	}
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close must complete after the in-flight write finishes")
	}
	// 此后不再需要慢写钩子（后续 stB 的写不再阻塞）。
	SetWriteHooks(WriteHooks{})

	// 写确实落盘；Close 后新 Store 可拿锁（锁已释放）、旧 Store 写被拒。
	if d, err := stA.Drafts.LoadDraft(1); err != nil || d != "慢写在途内容" {
		t.Fatalf("in-flight write must land before close releases the lock, got %q err=%v", d, err)
	}
	stB := NewStore(dir)
	if !stB.Ready() {
		t.Fatalf("store B must acquire the workspace after close: %v", stB.Init())
	}
	if err := stB.Drafts.SaveDraft(1, "新 Store 写入"); err != nil {
		t.Fatalf("store B write failed: %v", err)
	}
	if err := stA.Drafts.SaveDraft(1, "旧 Store 再写"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store write must fail with closed error, got: %v", err)
	}
	stB.Close()
}

// TestStoreClose_ConcurrentIdempotent 验证并发 Close 安全（独占 lease 串行 +
// closed.Swap 幂等）：多个 goroutine 同时 Close 不 panic、workspace 锁恰好释放
// 一次（随后新 Store 可正常拿锁）。
func TestStoreClose_ConcurrentIdempotent(t *testing.T) {
	dir := t.TempDir()
	stA := NewStore(dir)
	if err := stA.Init(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stA.Close()
		}()
	}
	wg.Wait()

	if stA.Ready() {
		t.Fatal("closed store must not be ready")
	}
	stB := NewStore(dir)
	if !stB.Ready() {
		t.Fatalf("store B must acquire after concurrent close: %v", stB.Init())
	}
	stB.Close()
}
