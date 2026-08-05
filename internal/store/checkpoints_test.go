package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func newTestCheckpointStore(t *testing.T) (*CheckpointStore, string) {
	t.Helper()
	dir := t.TempDir()
	io := newIO(dir)
	return NewCheckpointStore(io), dir
}

func TestCheckpointStore_AppendAndQuery(t *testing.T) {
	cs, _ := newTestCheckpointStore(t)

	cp1, err := cs.Append(domain.ChapterScope(1), "plan", "drafts/01.plan.json", "sha256:abc")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if cp1.Seq != 1 {
		t.Fatalf("seq want 1 got %d", cp1.Seq)
	}

	cp2, _ := cs.Append(domain.ChapterScope(1), "draft", "drafts/01.draft.md", "sha256:def")
	if cp2.Seq != 2 {
		t.Fatalf("seq want 2 got %d", cp2.Seq)
	}

	if got := cs.Latest(domain.ChapterScope(1)); got == nil || got.Step != "draft" {
		t.Fatalf("latest got %+v", got)
	}
	if got := cs.LatestByStep(domain.ChapterScope(1), "plan"); got == nil || got.Digest != "sha256:abc" {
		t.Fatalf("latestByStep plan got %+v", got)
	}
	if got := cs.LatestGlobal(); got == nil || got.Seq != 2 {
		t.Fatalf("latestGlobal got %+v", got)
	}
	if all := cs.All(); len(all) != 2 {
		t.Fatalf("all len want 2 got %d", len(all))
	}
}

// TestCheckpointStore_AppendPolish 验证 polish checkpoint 的附加元数据
// （input_digest/polisher_model/stage/changed）落盘与读取 round-trip。
// C1：AppendPolish 不做 digest 幂等去重——每次调用都追加新 checkpoint（新 seq），
// 顺序绑定（polish → consistency → critic）依赖每次 polish 都推进 seq。
func TestCheckpointStore_AppendPolish(t *testing.T) {
	cs, dir := newTestCheckpointStore(t)

	cp, err := cs.AppendPolish(
		domain.ChapterScope(3), "polish", "drafts/03.draft.md", "sha256:out",
		domain.PolishCheckpointMeta{
			InputDigest:   "sha256:in",
			PolisherModel: "mimo-polisher",
			Stage:         "draft",
			Changed:       true,
		},
	)
	if err != nil {
		t.Fatalf("AppendPolish: %v", err)
	}
	if cp.Digest != "sha256:out" || cp.InputDigest != "sha256:in" ||
		cp.PolisherModel != "mimo-polisher" || cp.Stage != "draft" || !cp.Changed {
		t.Fatalf("polish metadata lost: %+v", cp)
	}

	// 相同 scope+step+digest 再次写入必须追加新记录（新 seq），不得去重返回旧记录。
	cp2, err := cs.AppendPolish(
		domain.ChapterScope(3), "polish", "drafts/03.draft.md", "sha256:out",
		domain.PolishCheckpointMeta{InputDigest: "sha256:in", PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
	)
	if err != nil {
		t.Fatalf("AppendPolish second: %v", err)
	}
	if cp2.Seq <= cp.Seq {
		t.Fatalf("AppendPolish must always append a new checkpoint: seq %d vs %d", cp2.Seq, cp.Seq)
	}
	if all := cs.All(); len(all) != 2 {
		t.Fatalf("expected 2 polish checkpoints after no-dedup appends, got %d", len(all))
	}

	// 磁盘 round-trip：新实例从 jsonl 恢复元数据（最新一条）
	cs2 := NewCheckpointStore(newIO(dir))
	got := cs2.LatestByStep(domain.ChapterScope(3), "polish")
	if got == nil {
		t.Fatal("polish checkpoint missing after restore")
	}
	if got.Seq != cp2.Seq || got.InputDigest != "sha256:in" || got.PolisherModel != "mimo-polisher" || got.Stage != "draft" || !got.Changed {
		t.Fatalf("polish metadata lost after restore: %+v", got)
	}
}

// TestCheckpointStore_AppendAlways 验证 AppendAlways 每次调用都追加新 checkpoint
// （不做 digest 幂等去重），供 consistency_check 的顺序绑定使用。
func TestCheckpointStore_AppendAlways(t *testing.T) {
	cs, _ := newTestCheckpointStore(t)
	scope := domain.ChapterScope(1)

	cp1, err := cs.AppendAlways(scope, "consistency_check", "drafts/01.draft.md", "sha256:same")
	if err != nil {
		t.Fatalf("AppendAlways: %v", err)
	}
	cp2, err := cs.AppendAlways(scope, "consistency_check", "drafts/01.draft.md", "sha256:same")
	if err != nil {
		t.Fatalf("AppendAlways second: %v", err)
	}
	if cp2.Seq <= cp1.Seq {
		t.Fatalf("AppendAlways must always append: seq %d vs %d", cp2.Seq, cp1.Seq)
	}
	// 对照组：Append 相同 digest 仍幂等去重（全局语义不变）
	cp3, err := cs.Append(scope, "consistency_check", "drafts/01.draft.md", "sha256:same")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if cp3.Seq != cp2.Seq {
		t.Fatalf("Append must dedup same digest: seq %d vs %d", cp3.Seq, cp2.Seq)
	}
}

// TestCheckpointStore_BySeq 验证按 seq 查询。
func TestCheckpointStore_BySeq(t *testing.T) {
	cs, _ := newTestCheckpointStore(t)
	cp1, _ := cs.Append(domain.ChapterScope(1), "plan", "a", "sha256:1")
	cp2, _ := cs.Append(domain.ChapterScope(1), "draft", "b", "sha256:2")

	if got := cs.BySeq(cp1.Seq); got == nil || got.Step != "plan" {
		t.Fatalf("BySeq(%d) = %+v, want plan", cp1.Seq, got)
	}
	if got := cs.BySeq(cp2.Seq); got == nil || got.Step != "draft" {
		t.Fatalf("BySeq(%d) = %+v, want draft", cp2.Seq, got)
	}
	if got := cs.BySeq(cp2.Seq + 999); got != nil {
		t.Fatalf("BySeq(unknown) = %+v, want nil", got)
	}
}

func TestCheckpointStore_Idempotent(t *testing.T) {
	cs, dir := newTestCheckpointStore(t)

	cp1, _ := cs.Append(domain.ChapterScope(1), "plan", "drafts/01.plan.json", "sha256:abc")
	cp2, err := cs.Append(domain.ChapterScope(1), "plan", "drafts/01.plan.json", "sha256:abc")
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	if cp1.Seq != cp2.Seq {
		t.Fatalf("idempotent should return same seq, got %d vs %d", cp1.Seq, cp2.Seq)
	}
	if all := cs.All(); len(all) != 1 {
		t.Fatalf("cache should hold 1 entry, got %d", len(all))
	}

	// 磁盘上也应只有一行
	data, _ := os.ReadFile(filepath.Join(dir, checkpointsFile))
	if got := countLines(data); got != 1 {
		t.Fatalf("disk should have 1 line, got %d", got)
	}
}

func TestCheckpointStore_EmptyDigestNotIdempotent(t *testing.T) {
	cs, _ := newTestCheckpointStore(t)

	// 空 digest 不参与幂等去重
	cs.Append(domain.GlobalScope(), "note", "", "")
	cs.Append(domain.GlobalScope(), "note", "", "")
	if all := cs.All(); len(all) != 2 {
		t.Fatalf("empty digest should append both, got %d", len(all))
	}
}

func TestCheckpointStore_Reset(t *testing.T) {
	cs, dir := newTestCheckpointStore(t)
	cs.Append(domain.ChapterScope(1), "plan", "p", "sha256:1")
	cs.Append(domain.ChapterScope(1), "draft", "d", "sha256:2")

	if err := cs.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if all := cs.All(); len(all) != 0 {
		t.Fatalf("cache should be empty after reset, got %d", len(all))
	}
	if cs.LatestGlobal() != nil {
		t.Fatalf("latestGlobal should be nil after reset")
	}
	if _, err := os.Stat(filepath.Join(dir, checkpointsFile)); !os.IsNotExist(err) {
		t.Fatalf("file should be removed, err=%v", err)
	}

	// Reset 后 seq 重置：下次追加从 1 开始
	cp, _ := cs.Append(domain.ChapterScope(1), "plan", "p", "sha256:1")
	if cp.Seq != 1 {
		t.Fatalf("seq after reset should restart at 1, got %d", cp.Seq)
	}
}

func TestCheckpointStore_RestoreFromDisk(t *testing.T) {
	dir := t.TempDir()
	io1 := newIO(dir)
	cs1 := NewCheckpointStore(io1)
	cs1.Append(domain.ChapterScope(1), "plan", "p", "sha256:1")
	cs1.Append(domain.ChapterScope(1), "draft", "d", "sha256:2")
	cs1.Append(domain.ChapterScope(2), "plan", "p2", "sha256:3")

	// 模拟重启：新实例从同一目录加载
	io2 := newIO(dir)
	cs2 := NewCheckpointStore(io2)

	if all := cs2.All(); len(all) != 3 {
		t.Fatalf("restored cache len want 3 got %d", len(all))
	}
	if got := cs2.LatestGlobal(); got == nil || got.Seq != 3 {
		t.Fatalf("restored latestGlobal seq want 3 got %+v", got)
	}

	// seq 应从 4 续接，且幂等仍生效
	cp, _ := cs2.Append(domain.ChapterScope(2), "draft", "d2", "sha256:4")
	if cp.Seq != 4 {
		t.Fatalf("restored seq continuation want 4 got %d", cp.Seq)
	}
	dup, _ := cs2.Append(domain.ChapterScope(1), "plan", "p", "sha256:1")
	if dup.Seq != 1 {
		t.Fatalf("idempotent across restart, want seq 1 got %d", dup.Seq)
	}
}

func TestCheckpointStore_AllReturnsCopy(t *testing.T) {
	cs, _ := newTestCheckpointStore(t)
	cs.Append(domain.ChapterScope(1), "plan", "p", "sha256:1")

	all := cs.All()
	all[0].Step = "tampered"

	if got := cs.LatestGlobal(); got.Step != "plan" {
		t.Fatalf("internal cache should be immune to caller mutation, got %q", got.Step)
	}
}

func TestCheckpointStore_ConcurrentAppend(t *testing.T) {
	cs, _ := newTestCheckpointStore(t)

	const goroutines = 10
	const perGoroutine = 20

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := range perGoroutine {
				cs.Append(domain.ChapterScope(gid*100+i), "plan", "p", "")
			}
		}(g)
	}
	wg.Wait()

	all := cs.All()
	if len(all) != goroutines*perGoroutine {
		t.Fatalf("concurrent append lost data: want %d got %d", goroutines*perGoroutine, len(all))
	}

	// seq 应为 1..N，无重复
	seen := make(map[int64]bool, len(all))
	for _, cp := range all {
		if seen[cp.Seq] {
			t.Fatalf("duplicate seq %d", cp.Seq)
		}
		seen[cp.Seq] = true
	}
	for i := int64(1); i <= int64(len(all)); i++ {
		if !seen[i] {
			t.Fatalf("seq %d missing", i)
		}
	}
}

func TestCheckpointStore_SeqNotConsumedOnWriteFailure(t *testing.T) {
	cs, dir := newTestCheckpointStore(t)
	if _, err := cs.Append(domain.ChapterScope(1), "plan", "p", "sha256:1"); err != nil {
		t.Fatalf("seed append: %v", err)
	}

	// 把 jsonl 文件本身改为只读，使下一次 OpenFile 写入失败
	jsonlPath := filepath.Join(dir, checkpointsFile)
	if err := os.Chmod(jsonlPath, 0o444); err != nil {
		t.Skipf("chmod readonly not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(jsonlPath, 0o644) })

	if _, err := cs.Append(domain.ChapterScope(2), "plan", "p", "sha256:2"); err == nil {
		t.Fatal("expected write failure on readonly file")
	}

	// cache 不应被污染
	if all := cs.All(); len(all) != 1 {
		t.Fatalf("cache leaked failed entry, len=%d", len(all))
	}

	// 恢复写权限，重试应得 seq=2 而不是 seq=3
	if err := os.Chmod(jsonlPath, 0o644); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	cp, err := cs.Append(domain.ChapterScope(2), "plan", "p", "sha256:2")
	if err != nil {
		t.Fatalf("retry append: %v", err)
	}
	if cp.Seq != 2 {
		t.Fatalf("seq should not be consumed by failed append, want 2 got %d", cp.Seq)
	}
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}
