package store

import (
	"bufio"
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
func NewCheckpointStore(io *IO) *CheckpointStore {
	cs := &CheckpointStore{io: io}
	cs.loadFromDisk()
	return cs
}

// loadFromDisk 一次性把磁盘 jsonl 读进 cache 并恢复 seqGen。
func (cs *CheckpointStore) loadFromDisk() {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()

	cs.cache = readCheckpointsFile(cs.io.path(checkpointsFile))
	var maxSeq int64
	for _, cp := range cs.cache {
		if cp.Seq > maxSeq {
			maxSeq = cp.Seq
		}
	}
	cs.seqGen.Store(maxSeq)
}

// Append 追加一条 checkpoint。
// 幂等：相同 Scope + Step + Digest 已存在则跳过写入，直接返回已有记录。
func (cs *CheckpointStore) Append(scope domain.Scope, step, artifact, digest string) (*domain.Checkpoint, error) {
	return cs.append(scope, step, artifact, digest, true, nil)
}

// AppendAlways 追加一条 checkpoint，不做 digest 幂等去重：每次调用都产生新的 seq。
// 供"每次执行都必须推进 seq"的事件类检查点使用（如 check_consistency 的
// consistency_check——review_style 依赖 consistency seq > polish seq 的顺序绑定）。
func (cs *CheckpointStore) AppendAlways(scope domain.Scope, step, artifact, digest string) (*domain.Checkpoint, error) {
	return cs.append(scope, step, artifact, digest, false, nil)
}

// append 是 Append/AppendAlways/AppendPolish 的共享实现。
// dedup=true 时按 Scope+Step+Digest 幂等；polishMeta 非 nil 时附加精修元数据。
func (cs *CheckpointStore) append(scope domain.Scope, step, artifact, digest string, dedup bool, polishMeta *domain.PolishCheckpointMeta) (*domain.Checkpoint, error) {
	cs.io.mu.Lock()
	defer cs.io.mu.Unlock()

	if dedup && digest != "" {
		for i := len(cs.cache) - 1; i >= 0; i-- {
			cp := cs.cache[i]
			if cp.Scope.Matches(scope) && cp.Step == step && cp.Digest == digest {
				return &cp, nil
			}
		}
	}

	// seq 写成功后才推进，避免写失败留下永久跳号。
	// 已持 io.mu 写锁，Load+Store 之间不会被并发抢占。
	seq := cs.seqGen.Load() + 1
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
	return cs.append(scope, step, artifact, digest, false, &meta)
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
	for i := len(cs.cache) - 1; i >= 0; i-- {
		cp := cs.cache[i]
		if cp.Scope.Matches(scope) && cp.Step == step {
			return &cp
		}
	}
	return nil
}

// BySeq 按 seq 返回 checkpoint；不存在时返回 nil。
func (cs *CheckpointStore) BySeq(seq int64) *domain.Checkpoint {
	cs.io.mu.RLock()
	defer cs.io.mu.RUnlock()
	for i := len(cs.cache) - 1; i >= 0; i-- {
		cp := cs.cache[i]
		if cp.Seq == seq {
			return &cp
		}
	}
	return nil
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
