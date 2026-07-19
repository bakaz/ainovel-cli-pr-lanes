package projectprofile

import "regexp"

// ProfileVersion 是 project_profile.json 的当前模式版本。
// 版本不匹配必须 fail closed，绝不回落 Core4。
const ProfileVersion = "v1"

const SceneBeatV3ProfileID = "scene-beat-v3"

var manifestSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ProfileMarker 是 meta/project_profile.json 的磁盘形状。
// 由 Phase 3 的迁移 apply 负责写入；Phase 1 和 Phase 2 只读。
// marker 不得包含 policy/required-field 等可降级开关。
type ProfileMarker struct {
	Version                string       `json:"version"`
	Contract               string       `json:"contract"`
	Status                 string       `json:"status"`
	ProfileID              string       `json:"profile_id,omitempty"`
	EnrollmentFingerprint  *Fingerprint `json:"enrollment_fingerprint,omitempty"`
	ApprovedManifestSHA256 string       `json:"approved_manifest_sha256,omitempty"`
}
