package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

const checkpointsFile = "meta/checkpoints.jsonl"

// CheckpointStore 管理 step 级 checkpoint 的追加与查询。
// 磁盘格式：meta/checkpoints.jsonl，只追加；查询走内存镜像。
// 不变量：cache 是 checkpoints.jsonl 的镜像，由 Append/Reset 单点维护。
// 并发：cache 受 io.mu 保护，写走 Lock、读走 RLock。
type CheckpointStore struct {
	io     *IO
	seqGen atomic.Int64
	cache  []domain.Checkpoint
}

// NewCheckpointStore 创建 checkpoint 存储，从磁盘一次性加载已有 checkpoint 到 cache。
// P0-2 fail-closed：磁盘数据损坏（重复 seq / 序号倒退）时返回明确错误，不继续运行。
func NewCheckpointStore(io *IO) (*CheckpointStore, error) {
	cs := &CheckpointStore{io: io}
	if err := cs.loadFromDisk(); err != nil {
		return nil, err
	}
	return cs, nil
}

// loadFromDisk 一次性把磁盘 jsonl 读进 cache 并恢复 seqGen。
// P0-2：加载即检测数据损坏（重复 seq / 序号倒退 / 同 seq 不同 step 身份），
// 命中即 fail-closed——不带着损坏的 seq 空间继续运行（历史死锁根因：重复 seq
// 导致 BySeq 取错记录、LatestByStep 顺序绑定错乱）。
func (cs *CheckpointStore) loadFromDisk() error {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()

	cps := readCheckpointsFile(cs.io.path(checkpointsFile))
	if err := validateCheckpointSequence(cps); err != nil {
		return err
	}
	cs.cache = cps
	var maxSeq int64
	for _, cp := range cps {
		if cp.Seq > maxSeq {
			maxSeq = cp.Seq
		}
	}
	cs.seqGen.Store(maxSeq)
	return nil
}

// validateCheckpointSequence 检查 checkpoint 序列完整性（P0-2，fail closed）：
// 重复 seq / 序号倒退 / 同 seq 不同 step 身份 → 数据损坏错误，不继续运行。
// 无 seq 字段的记录（Seq==0，旧格式/手工 seed 记录，如 seeded arc_summary）
// 不参与序列校验——它们没有 seq 身份，不影响 BySeq/LatestByStep 的 seq 语义。
func validateCheckpointSequence(cps []domain.Checkpoint) error {
	stepBySeq := make(map[int64]string, len(cps))
	prevSeq := int64(0)
	seenNonZero := false
	for _, cp := range cps {
		if cp.Seq == 0 {
			continue
		}
		if seenNonZero && cp.Seq <= prevSeq {
			if cp.Seq == prevSeq {
				return fmt.Errorf("checkpoint 数据损坏：seq=%d 重复（step=%s / step=%s）",
					cp.Seq, stepBySeq[cp.Seq], cp.Step)
			}
			return fmt.Errorf("checkpoint 数据损坏：seq=%d 序号倒退（前一条 seq=%d，step=%s）",
				cp.Seq, prevSeq, cp.Step)
		}
		stepBySeq[cp.Seq] = cp.Step
		prevSeq = cp.Seq
		seenNonZero = true
	}
	return nil
}

// Append 追加一条 checkpoint。
// 幂等：相同 Scope + Step + Digest 已存在则跳过写入，直接返回已有记录。
func (cs *CheckpointStore) Append(scope domain.Scope, step, artifact, digest string) (*domain.Checkpoint, error) {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()
	return cs.appendUnlocked(scope, step, artifact, digest, true, nil)
}

// AppendAlways 追加一条 checkpoint，不做 digest 幂等去重：每次调用都产生新的 seq。
// 供"每次执行都必须推进 seq"的事件类检查点使用（如 check_consistency 的
// consistency_check——review_style 依赖 consistency seq > polish seq 的顺序绑定）。
func (cs *CheckpointStore) AppendAlways(scope domain.Scope, step, artifact, digest string) (*domain.Checkpoint, error) {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()
	return cs.appendUnlocked(scope, step, artifact, digest, false, nil)
}

// appendUnlocked 是 Append/AppendAlways/AppendPolish 的共享实现（调用方必须已持有
// cs.io.mu 写锁；CommitPolishCandidate 在同一临界区内复用此实现）。
// dedup=true 时按 Scope+Step+Digest 幂等；polishMeta 非 nil 时附加精修元数据。
func (cs *CheckpointStore) appendUnlocked(scope domain.Scope, step, artifact, digest string, dedup bool, polishMeta *domain.PolishCheckpointMeta) (*domain.Checkpoint, error) {
	if dedup && digest != "" {
		for i := len(cs.cache) - 1; i >= 0; i-- {
			cp := cs.cache[i]
			if cp.Scope.Matches(scope) && cp.Step == step && cp.Digest == digest {
				return &cp, nil
			}
		}
	}

	// P0-2：seq 分配以持久化尾部为准（写锁内重新读取文件最后一条的 seq），
	// 与内存 cache 取较大者 +1——即使内存 cache 与磁盘不同步（历史多 Store 实例
	// 交错写入、本进程 cache 陈旧），也不会分配重复 seq（配合 P0-1 单写者锁
	// 后不并发，此处是防御性双保险）。seq 写成功后才推进，避免写失败留下跳号。
	lastSeq, err := readLastCheckpointSeq(cs.io.path(checkpointsFile))
	if err != nil {
		return nil, err
	}
	memSeq := cs.seqGen.Load()
	if lastSeq > memSeq {
		memSeq = lastSeq
	}
	seq := memSeq + 1
	cp := domain.Checkpoint{
		Seq:        seq,
		Scope:      scope,
		Step:       step,
		Artifact:   artifact,
		Digest:     digest,
		OccurredAt: time.Now(),
	}
	if polishMeta != nil {
		cp.InputDigest = polishMeta.InputDigest
		cp.PolisherModel = polishMeta.PolisherModel
		cp.Stage = polishMeta.Stage
		cp.Changed = polishMeta.Changed
		cp.Degraded = polishMeta.Degraded
		cp.ErrorCategory = polishMeta.ErrorCategory
		cp.Method = polishMeta.Method
		cp.EditCount = polishMeta.EditCount
		cp.ProposedEditCount = polishMeta.ProposedEditCount
		cp.DroppedEditCount = polishMeta.DroppedEditCount
		cp.DropReasons = polishMeta.DropReasons
		cp.NormalizedMatchCount = polishMeta.NormalizedMatchCount
		cp.Partial = polishMeta.Partial
		cp.MatchModes = polishMeta.MatchModes
		// 候选工具协议审计字段（schema §9；旧路径为零值，omitempty 不落盘）。
		cp.OperationID = polishMeta.OperationID
		cp.RunRejectionCode = polishMeta.RunRejectionCode
		cp.PlanIssueCount = polishMeta.PlanIssueCount
		cp.BatchCount = polishMeta.BatchCount
		cp.FinishStatus = polishMeta.FinishStatus
		cp.UnresolvedCount = polishMeta.UnresolvedCount
		cp.PlanDigest = polishMeta.PlanDigest
	}

	data, err := json.Marshal(cp)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := cs.io.AppendLineUnlocked(checkpointsFile, data); err != nil {
		return nil, err
	}
	cs.seqGen.Store(seq)
	cs.cache = append(cs.cache, cp)
	return &cp, nil
}

// AppendArtifact 计算 artifact 内容指纹后追加 checkpoint。
func (cs *CheckpointStore) AppendArtifact(scope domain.Scope, step, artifact string) (*domain.Checkpoint, error) {
	if artifact == "" {
		return cs.Append(scope, step, "", "")
	}
	data, err := cs.io.ReadFile(artifact)
	if err != nil {
		return nil, fmt.Errorf("digest artifact %s: %w", artifact, err)
	}
	sum := sha256.Sum256(data)
	return cs.Append(scope, step, artifact, "sha256:"+hex.EncodeToString(sum[:]))
}

// AppendPolish 追加一条带精修元数据的 checkpoint（polish_draft 工具专用）。
// 不做 digest 幂等去重：每次成功调用都追加新 checkpoint（新 seq）——顺序绑定
// （polish → consistency → critic）依赖每次 polish 都推进 seq。
func (cs *CheckpointStore) AppendPolish(scope domain.Scope, step, artifact, digest string, meta domain.PolishCheckpointMeta) (*domain.Checkpoint, error) {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()
	return cs.appendUnlocked(scope, step, artifact, digest, false, &meta)
}

// Latest 返回指定 scope 的最新 checkpoint。
func (cs *CheckpointStore) Latest(scope domain.Scope) *domain.Checkpoint {
	cs.io.mu.RLock()
	defer cs.io.mu.RUnlock()
	for i := len(cs.cache) - 1; i >= 0; i-- {
		if cs.cache[i].Scope.Matches(scope) {
			cp := cs.cache[i]
			return &cp
		}
	}
	return nil
}

// LatestByStep 返回指定 scope + step 的最新 checkpoint。
func (cs *CheckpointStore) LatestByStep(scope domain.Scope, step string) *domain.Checkpoint {
	cs.io.mu.RLock()
	defer cs.io.mu.RUnlock()
	return cs.latestByStepUnlocked(scope, step)
}

// latestByStepUnlocked 是 LatestByStep 的无锁实现（调用方必须已持有 cs.io.mu
// 读锁或写锁；CommitPolishCandidate/CommitReviewResult 在同一临界区内复用）。
func (cs *CheckpointStore) latestByStepUnlocked(scope domain.Scope, step string) *domain.Checkpoint {
	for i := len(cs.cache) - 1; i >= 0; i-- {
		cp := cs.cache[i]
		if cp.Scope.Matches(scope) && cp.Step == step {
			return &cp
		}
	}
	return nil
}

// BySeq 按 seq 返回 checkpoint。
// 返回 (nil, nil) 表示不存在；P0-2：发现重复 seq（数据损坏）→ 返回明确错误，
// 不再任取一条（历史根因：重复 seq 下 BySeq 可能取到错误记录）。
func (cs *CheckpointStore) BySeq(seq int64) (*domain.Checkpoint, error) {
	cs.io.mu.RLock()
	defer cs.io.mu.RUnlock()
	var found *domain.Checkpoint
	for i := len(cs.cache) - 1; i >= 0; i-- {
		cp := cs.cache[i]
		if cp.Seq == seq {
			if found != nil {
				return nil, fmt.Errorf("checkpoint 数据损坏：seq=%d 重复（step=%s / step=%s）",
					seq, found.Step, cp.Step)
			}
			c := cp
			found = &c
		}
	}
	return found, nil
}

// LatestGlobal 返回全局最新 checkpoint（不区分 scope）。
func (cs *CheckpointStore) LatestGlobal() *domain.Checkpoint {
	cs.io.mu.RLock()
	defer cs.io.mu.RUnlock()
	if len(cs.cache) == 0 {
		return nil
	}
	cp := cs.cache[len(cs.cache)-1]
	return &cp
}

// All 返回全部 checkpoint 列表副本（按 seq 递增）。
func (cs *CheckpointStore) All() []domain.Checkpoint {
	cs.io.mu.RLock()
	defer cs.io.mu.RUnlock()
	if len(cs.cache) == 0 {
		return nil
	}
	out := make([]domain.Checkpoint, len(cs.cache))
	copy(out, cs.cache)
	return out
}

// Reset 清空 checkpoint 文件与 cache。仅在新建小说时使用。
// 先删文件再清内存：删除失败时保留 cache 与 seqGen，避免内存与磁盘状态错位。
func (cs *CheckpointStore) Reset() error {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()
	if err := cs.io.RemoveFileUnlocked(checkpointsFile); err != nil {
		return err
	}
	cs.seqGen.Store(0)
	cs.cache = nil
	return nil
}

// readCheckpointsFile 解析 jsonl；跳过格式错误行以容忍尾部截断。
func readCheckpointsFile(path string) []domain.Checkpoint {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var result []domain.Checkpoint
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var cp domain.Checkpoint
		if json.Unmarshal(line, &cp) == nil {
			result = append(result, cp)
		}
	}
	return result
}

// readLastCheckpointSeq 读取 jsonl 文件最后一条可解析记录的 seq（P0-2 的
// seq 分配持久化来源）。文件不存在/为空 → 0。从文件尾部向前扫描，跳过空行与
// 不可解析的截断尾部（崩溃中断的半个 JSON），取最近一条完整记录。
func readLastCheckpointSeq(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if size == 0 {
		return 0, nil
	}

	// 单条 checkpoint JSON 远小于 64KB；向后扫描窗口覆盖最后一条记录
	// （含截断尾部回溯到上一条完整记录）绰绰有余。
	const maxBackScan = 64 * 1024
	readSize := size
	if readSize > maxBackScan {
		readSize = maxBackScan
	}
	buf := make([]byte, readSize)
	if _, err := f.ReadAt(buf, size-readSize); err != nil {
		return 0, err
	}

	end := len(buf)
	for end > 0 {
		i := bytes.LastIndexByte(buf[:end], '\n')
		var line []byte
		if i < 0 {
			line = buf[:end]
			end = 0
		} else {
			line = buf[i+1 : end]
			end = i
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var cp domain.Checkpoint
		if err := json.Unmarshal(line, &cp); err != nil {
			continue // 截断/损坏尾部：继续向前找上一条完整记录
		}
		return cp.Seq, nil
	}
	return 0, nil
}
