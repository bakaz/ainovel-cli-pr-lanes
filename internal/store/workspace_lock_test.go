package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// TestWorkspaceLock_SingleWritablePerProcess 验证复核阻塞项 2（方案 A）：
// 同一进程内同一 workspace 只允许一个可写 Store；第二个构造即失败（不加载
// cache、写操作 fail-closed）；释放后可重新获取；锁文件本身不参与数据读写。
func TestWorkspaceLock_SingleWritablePerProcess(t *testing.T) {
	dir := t.TempDir()
	stA := NewStore(dir)
	if err := stA.Init(); err != nil {
		t.Fatalf("first store Init: %v", err)
	}
	defer stA.Close()

	// 锁文件存在且保持为空（不参与数据读写）。
	lockPath := filepath.Join(dir, workspaceLockFileName)
	if st, err := os.Stat(lockPath); err != nil || st.Size() != 0 {
		t.Fatalf("lock file should exist and stay empty, stat err=%v size=%d", err, st.Size())
	}

	// 第二个可写 Store：明确错误 + 未 ready + 不加载 checkpoint cache。
	stB := NewStore(dir)
	if stB.Ready() {
		t.Fatal("second writable store in same process must not be ready")
	}
	if err := stB.Init(); err == nil || !strings.Contains(err.Error(), "本进程内另一个 Store 实例占用") {
		t.Fatalf("expected in-process busy error, got: %v", err)
	}
	if all := stB.Checkpoints.All(); len(all) != 0 {
		t.Fatalf("lock-failed store must not load cache, got %d entries", len(all))
	}
	// 复核阻塞项 4：未 ready Store 的写操作 fail-closed。
	if err := stB.Drafts.SaveDraft(1, "x"); err == nil {
		t.Fatal("unready store draft write must fail")
	}
	if _, err := stB.Checkpoints.Append(domain.ChapterScope(1), "plan", "a", "sha256:1"); err == nil {
		t.Fatal("unready store checkpoint append must fail")
	}

	// 释放后重新获取成功（注册表条目已移除）。
	stA.Close()
	stC := NewStore(dir)
	if !stC.Ready() {
		t.Fatalf("store after full release must be ready: %v", stC.Init())
	}
	stC.Close()
}

// TestWorkspaceLock_OrderLockBeforeCacheLoad 验证复核阻塞项 1：锁失败（本进程
// 双写）的 Store 不加载 checkpoint cache（锁未到手不初始化持久化 cache）；
// 释放后新实例锁到手并正常加载 cache。
func TestWorkspaceLock_OrderLockBeforeCacheLoad(t *testing.T) {
	dir := t.TempDir()
	stA := NewStore(dir)
	if err := stA.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := stA.Checkpoints.Append(domain.ChapterScope(1), "plan", "a", "sha256:1"); err != nil {
		t.Fatal(err)
	}

	// B 实例在 A 释放前无法取得锁，且不加载 cache（阻塞项 1：锁到手前不读盘）。
	stB := NewStore(dir)
	if all := stB.Checkpoints.All(); len(all) != 0 {
		t.Fatalf("B must not load cache before acquiring the lock, got %d entries", len(all))
	}

	// A 释放后，新实例锁到手 + cache 正常加载。
	stA.Close()
	stC := NewStore(dir)
	if !stC.Ready() {
		t.Fatalf("store after release must be ready: %v", stC.Init())
	}
	if all := stC.Checkpoints.All(); len(all) != 1 {
		t.Fatalf("fresh store must load cache from disk, got %d entries", len(all))
	}
	stC.Close()
}

// TestWorkspaceLock_ReadOnlyStore 验证复核阻塞项 2 只读模式：NewReadOnlyStore
// 不取 workspace 锁（可与可写 Store 并存、不阻挡真实写入进程）、可正常读取
// （含 checkpoint cache 加载）、写操作 fail-closed；且不占用"单写者"槽位。
func TestWorkspaceLock_ReadOnlyStore(t *testing.T) {
	dir := t.TempDir()
	stW := NewStore(dir)
	if err := stW.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := stW.Checkpoints.Append(domain.ChapterScope(1), "plan", "a", "sha256:1"); err != nil {
		t.Fatal(err)
	}
	if err := stW.Drafts.SaveDraft(1, "正文"); err != nil {
		t.Fatal(err)
	}

	stR := NewReadOnlyStore(dir)
	if !stR.Ready() {
		t.Fatalf("read-only store should be ready for reads: %v", stR.Init())
	}
	if !stR.ReadOnly() {
		t.Fatal("ReadOnly() must report true")
	}
	// 读取正常（含 checkpoint cache 加载）。
	if all := stR.Checkpoints.All(); len(all) != 1 {
		t.Fatalf("read-only store must load cache, got %d entries", len(all))
	}
	if d, err := stR.Drafts.LoadDraft(1); err != nil || d != "正文" {
		t.Fatalf("read-only store read failed: %q %v", d, err)
	}
	// 写操作 fail-closed。
	if err := stR.Drafts.SaveDraft(1, "覆盖"); err == nil {
		t.Fatal("read-only store draft write must fail")
	}
	if err := stR.Progress.Save(&domain.Progress{}); err == nil {
		t.Fatal("read-only store progress write must fail")
	}
	if _, err := stR.Checkpoints.Append(domain.ChapterScope(2), "plan", "a", "sha256:2"); err == nil {
		t.Fatal("read-only store checkpoint append must fail")
	}

	// 只读 Store 不占用单写者槽位：可写 Store 释放后仍可重新获取。
	stW.Close()
	stW2 := NewStore(dir)
	if !stW2.Ready() {
		t.Fatalf("writable store must be acquirable while read-only store alive: %v", stW2.Init())
	}
	stW2.Close()
	stR.Close()
}

// TestWorkspaceLock_PlatformExclusive 直接操作平台锁句柄（绕过进程内注册表）
// 验证独占语义：同一 workspace 的第二个句柄必须拿不到锁
// （Windows LockFileEx / Unix flock 的跨句柄互斥——跨进程冲突的同一机制）。
func TestWorkspaceLock_PlatformExclusive(t *testing.T) {
	dir := t.TempDir()
	f1, err := openWorkspaceLockFile(dir)
	if err != nil {
		t.Fatalf("open first handle: %v", err)
	}
	defer f1.Close()
	if err := lockWorkspaceFileExclusive(f1); err != nil {
		t.Fatalf("lock first handle: %v", err)
	}

	f2, err := openWorkspaceLockFile(dir)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer f2.Close()
	if err := lockWorkspaceFileExclusive(f2); !errors.Is(err, errWorkspaceLockBusy) {
		t.Fatalf("second handle must fail with workspace-busy, got: %v", err)
	}
}

// TestWorkspaceLock_StoreInitFailClosed 验证 Store 集成：锁失败经 Init 返回
// 明确错误；同进程第二 Store 共享锁的旧语义已被"单写者拒绝"取代（方案 A）。
func TestWorkspaceLock_StoreInitFailClosed(t *testing.T) {
	dir := t.TempDir()
	st1 := NewStore(dir)
	if err := st1.Init(); err != nil {
		t.Fatalf("first store Init: %v", err)
	}
	st2 := NewStore(dir) // 同进程第二可写实例 → 未 ready
	if st2.Ready() {
		t.Fatal("second writable store must not be ready")
	}
	if err := st2.Init(); err == nil {
		t.Fatal("second store Init must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, workspaceLockFileName)); err != nil {
		t.Fatalf("lock file should exist after Init: %v", err)
	}
	st1.Close()
	st2.Close() // 未持有锁，Close 为空操作
}
