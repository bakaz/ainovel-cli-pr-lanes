package store

import (
	"encoding/json"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// PrefixManifestStore 追加式记录每次 LLM 请求的缓存分层诊断 manifest 到
// meta/prefix_manifest.jsonl。只记 hash 与 token 数（domain.PrefixManifest），
// 不记正文——纯观测面，append-only，不做读取/轮转。
type PrefixManifestStore struct{ io *IO }

func NewPrefixManifestStore(io *IO) *PrefixManifestStore { return &PrefixManifestStore{io: io} }

// Append 追加一条 manifest（JSONL 一行）。写入失败由调用方决定是否告警。
func (s *PrefixManifestStore) Append(m domain.PrefixManifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.io.AppendLine("meta/prefix_manifest.jsonl", data)
}
