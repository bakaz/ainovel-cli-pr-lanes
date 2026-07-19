package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ProfileData 是 meta/project_profile.json 的磁盘形状。
// Phase 1 只读（marker 由 Phase 3 迁移 apply 写入）。
// marker 不得包含 policy/required-field 等可降级开关。
// 解码使用 DisallowUnknownFields 确保严格解析。
type ProfileData struct {
	Version               string `json:"version"`
	Contract              string `json:"contract"`
	Status                string `json:"status"`
	ProfileID             string `json:"profile_id,omitempty"`
	EnrollmentFingerprint *struct {
		PremiseHash      string `json:"premise_hash"`
		ChaptersHash     string `json:"chapters_hash"`
		CompletedThrough int    `json:"completed_through"`
	} `json:"enrollment_fingerprint,omitempty"`
	ApprovedManifestSHA256 string `json:"approved_manifest_sha256,omitempty"`
}

// ProjectProfileStore 管理 meta/project_profile.json 的读写。
// 空 / 损坏 / 未知 profile / 版本不匹配必须 fail closed。
type ProjectProfileStore struct{ io *IO }

// NewProjectProfileStore 创建 ProjectProfileStore。
func NewProjectProfileStore(io *IO) *ProjectProfileStore {
	return &ProjectProfileStore{io: io}
}

// LoadRaw 读取原始 marker 数据。文件不存在时返回 (nil, nil)。
// 损坏 / 格式错误 / 未知字段返回 error（fail closed）。
func (s *ProjectProfileStore) LoadRaw() (*ProfileData, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadUnlocked()
}

func (s *ProjectProfileStore) loadUnlocked() (*ProfileData, error) {
	data, err := s.io.ReadFileUnlocked("meta/project_profile.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project_profile.json: %w", err)
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("project_profile.json is empty (zero bytes)")
	}

	var marker ProfileData
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&marker); err != nil {
		return nil, fmt.Errorf("parse project_profile.json: %w", err)
	}

	// 第二次 Decode 必须返回 io.EOF——拒绝所有非 EOF 的残余内容：
	// garbage, 孤立 ]/}, 第二个 JSON 文档。
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("project_profile.json: trailing JSON document after first object")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("project_profile.json: unexpected content after first JSON object: %v", err)
	}

	if marker.Version == "" {
		return nil, fmt.Errorf("project_profile.json: missing version")
	}

	return &marker, nil
}

// SaveRaw 写入 meta/project_profile.json（供 Phase 3 迁移 apply 使用）。
func (s *ProjectProfileStore) SaveRaw(marker ProfileData) error {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	return s.saveUnlocked(marker)
}

func (s *ProjectProfileStore) saveUnlocked(marker ProfileData) error {
	return s.io.WriteJSONUnlocked("meta/project_profile.json", marker)
}

// Exists 检查 project_profile.json 是否存在。
func (s *ProjectProfileStore) Exists() bool {
	_, err := os.Stat(s.io.path("meta/project_profile.json"))
	return err == nil
}
