package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func newTestCheckpointStore(t *testing.T) (*CheckpointStore, string) {
	t.Helper()
	dir := t.TempDir()
	io := newIO(dir)
	cs, err := NewCheckpointStore(io)
	if err != nil {
		t.Fatalf("NewCheckpointStore: %v", err)
	}
	return cs, dir
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
	cs2, err := NewCheckpointStore(newIO(dir))
	if err != nil {
		t.Fatalf("NewCheckpointStore: %v", err)
	}
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

	got, err := cs.BySeq(cp1.Seq)
	if err != nil {
		t.Fatalf("BySeq(%d): %v", cp1.Seq, err)
	}
	if got == nil || got.Step != "plan" {
		t.Fatalf("BySeq(%d) = %+v, want plan", cp1.Seq, got)
	}
	got, err = cs.BySeq(cp2.Seq)
	if err != nil {
		t.Fatalf("BySeq(%d): %v", cp2.Seq, err)
	}
	if got == nil || got.Step != "draft" {
		t.Fatalf("BySeq(%d) = %+v, want draft", cp2.Seq, got)
	}
	got, err = cs.BySeq(cp2.Seq + 999)
	if err != nil {
		t.Fatalf("BySeq(unknown): %v", err)
	}
	if got != nil {
		t.Fatalf("BySeq(unknown) = %+v, want nil", got)
	}
}

// TestCheckpointStore_BySeqDuplicate 验证重复 seq（数据损坏）时 BySeq 返回明确
// 错误而非任取一条（P0-2：历史根因——重复 seq 下 BySeq 可能取到错误记录）。
func TestCheckpointStore_BySeqDuplicate(t *testing.T) {
	dir := t.TempDir()
	// 手工构造重复 seq 文件：seq=1 出现两次（step=plan / step=polish）。
	lines := []string{
		`{"seq":1,"scope":{"kind":"chapter","chapter":1},"step":"plan","occurred_at":"2026-01-01T00:00:00Z"}`,
		`{"seq":1,"scope":{"kind":"chapter","chapter":1},"step":"polish","occurred_at":"2026-01-01T00:00:01Z"}`,
	}
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	jsonlPath := filepath.Join(dir, checkpointsFile)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonlPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	// 加载 fail-closed：NewCheckpointStore 直接报数据损坏错误。
	if _, err := NewCheckpointStore(newIO(dir)); err == nil {
		t.Fatal("expected corruption error on duplicate seq at load")
	} else if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("expected '重复' corruption error, got: %v", err)
	}

	// BySeq：手工构造重复 seq 的 cache（绕过加载校验，直接注入）。
	cs := &CheckpointStore{io: newIO(dir)}
	cs.cache = []domain.Checkpoint{
		{Seq: 1, Scope: domain.ChapterScope(1), Step: "plan"},
		{Seq: 1, Scope: domain.ChapterScope(1), Step: "polish"},
	}
	if _, err := cs.BySeq(1); err == nil {
		t.Fatal("expected corruption error on duplicate seq in BySeq")
	} else if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("expected '重复' corruption error, got: %v", err)
	}
}

// TestCheckpointStore_LoadNonMonotonic 验证序号倒退（数据损坏）时加载 fail-closed。
func TestCheckpointStore_LoadNonMonotonic(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"seq":1,"scope":{"kind":"chapter","chapter":1},"step":"plan","occurred_at":"2026-01-01T00:00:00Z"}`,
		`{"seq":3,"scope":{"kind":"chapter","chapter":1},"step":"draft","occurred_at":"2026-01-01T00:00:01Z"}`,
		`{"seq":2,"scope":{"kind":"chapter","chapter":1},"step":"polish","occurred_at":"2026-01-01T00:00:02Z"}`,
	}
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	jsonlPath := filepath.Join(dir, checkpointsFile)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonlPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewCheckpointStore(newIO(dir))
	if err == nil {
		t.Fatal("expected corruption error on non-monotonic seq at load")
	}
	if !strings.Contains(err.Error(), "倒退") {
		t.Fatalf("expected '倒退' corruption error, got: %v", err)
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
	cs1, err := NewCheckpointStore(io1)
	if err != nil {
		t.Fatalf("NewCheckpointStore: %v", err)
	}
	cs1.Append(domain.ChapterScope(1), "plan", "p", "sha256:1")
	cs1.Append(domain.ChapterScope(1), "draft", "d", "sha256:2")
	cs1.Append(domain.ChapterScope(2), "plan", "p2", "sha256:3")

	// 模拟重启：新实例从同一目录加载
	io2 := newIO(dir)
	cs2, err := NewCheckpointStore(io2)
	if err != nil {
		t.Fatalf("NewCheckpointStore: %v", err)
	}

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
