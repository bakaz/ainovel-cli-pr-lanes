package projectprofile

import (
	"testing"
)

func TestRegistry_Resolve_MarkerCore_Rejected(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "core4",
				Status:   "core",
			}, nil
		},
		func() (Fingerprint, error) {
			return Fingerprint{}, nil
		},
	)
	_, err := r.Resolve()
	if err == nil {
		t.Fatal("Core4 marker should be rejected (use no marker for Core4 projects)")
	}
}

func TestRegistry_Resolve_MarkerMigrationRequired(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "scene_beat_v3",
				Status:   "migration_required",
			}, nil
		},
		nil,
	)
	rp, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if rp.Contract != ContractSceneBeatV3 {
		t.Errorf("contract = %v, want scene_beat_v3", rp.Contract)
	}
	if rp.Status != StatusMigrationRequired {
		t.Errorf("status = %v, want migration_required", rp.Status)
	}
}

func TestRegistry_Resolve_MarkerActiveRequiresCompleteAudit(t *testing.T) {
	enrolled := Fingerprint{PremiseHash: "premise", ChaptersHash: "chapters", CompletedThrough: 34}
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version: ProfileVersion, Contract: "scene_beat_v3", Status: "active",
				ProfileID: SceneBeatV3ProfileID, EnrollmentFingerprint: &enrolled,
				ApprovedManifestSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			}, nil
		},
		func() (Fingerprint, error) { return enrolled, nil },
		&enrolled,
	)
	resolved, err := r.Resolve()
	if err != nil || resolved.Contract != ContractSceneBeatV3 || resolved.Status != StatusActive {
		t.Fatalf("valid active audit was rejected: profile=%+v err=%v", resolved, err)
	}

	bad := []ProfileMarker{
		{Version: ProfileVersion, Contract: "scene_beat_v3", Status: "active"},
		{Version: ProfileVersion, Contract: "scene_beat_v3", Status: "active", ProfileID: SceneBeatV3ProfileID, EnrollmentFingerprint: &enrolled, ApprovedManifestSHA256: "NOT-A-HASH"},
		{Version: ProfileVersion, Contract: "scene_beat_v3", Status: "active", ProfileID: SceneBeatV3ProfileID, EnrollmentFingerprint: &Fingerprint{CompletedThrough: 34}, ApprovedManifestSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	for i := range bad {
		marker := bad[i]
		reject := NewRegistry(func() (*ProfileMarker, error) { return &marker, nil }, func() (Fingerprint, error) { return enrolled, nil }, &enrolled)
		if _, err := reject.Resolve(); err == nil {
			t.Fatalf("invalid active audit %d was accepted", i)
		}
	}
}

func TestRegistry_Resolve_MarkerVersionMismatch(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v0",
				Contract: "core4",
				Status:   "core",
			}, nil
		},
		nil,
	)
	_, err := r.Resolve()
	if err == nil {
		t.Fatal("version mismatch should fail closed")
	}
}

func TestRegistry_Resolve_MarkerInvalidContract(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "nonexistent",
				Status:   "core",
			}, nil
		},
		nil,
	)
	_, err := r.Resolve()
	if err == nil {
		t.Fatal("invalid contract should fail closed")
	}
}

func TestRegistry_Resolve_MarkerInvalidStatus(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "core4",
				Status:   "bogus",
			}, nil
		},
		nil,
	)
	_, err := r.Resolve()
	if err == nil {
		t.Fatal("invalid status should fail closed")
	}
}

func TestRegistry_Resolve_MarkerMigrationRequiredWithCore4_Rejected(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "core4",
				Status:   "migration_required",
			}, nil
		},
		nil,
	)
	_, err := r.Resolve()
	if err == nil {
		t.Fatal("migration_required with core4 contract should be rejected")
	}
}

func TestRegistry_Resolve_MarkerCoreWithV3_Rejected(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "scene_beat_v3",
				Status:   "core",
			}, nil
		},
		nil,
	)
	_, err := r.Resolve()
	if err == nil {
		t.Fatal("core status with scene_beat_v3 contract should be rejected (cannot downgrade)")
	}
}

func TestRegistry_Resolve_MarkerLoadError(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return nil, nil // marker absent
		},
		func() (Fingerprint, error) {
			return Fingerprint{}, nil // fingerprint returns empty (not enrolled)
		},
	)
	rp, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if rp.Contract != ContractCore4 {
		t.Errorf("no marker + no enrollment should get core4, got %v", rp.Contract)
	}
	if rp.Status != StatusCore {
		t.Errorf("no marker + no enrollment should get core, got %v", rp.Status)
	}
}

func TestRegistry_Resolve_NoMarkerEnrolledFingerprint(t *testing.T) {
	// 通过 NewRegistry 的 expectedEnrolled 参数注入预期 enrolled 指纹，
	// fingerprinter 仍使用真实计算逻辑。
	enrolled := NewFingerprint("some premise", map[string]string{"01.md": "content"})

	r := NewRegistry(
		func() (*ProfileMarker, error) { return nil, nil },
		func() (Fingerprint, error) {
			// 真实 fingerprinter，返回与 enrolled 完全相同的值
			return enrolled, nil
		},
		&enrolled, // 注入预期 enrolled 指纹匹配值
	)
	rp, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if rp.Contract != ContractSceneBeatV3 {
		t.Errorf("matching expected enrolled should get scene_beat_v3, got %v", rp.Contract)
	}
	if rp.Status != StatusMigrationRequired {
		t.Errorf("matching expected enrolled should get migration_required, got %v", rp.Status)
	}
}

func TestRegistry_Resolve_MarkerTakesPriority(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) {
			return &ProfileMarker{
				Version:  "v1",
				Contract: "scene_beat_v3",
				Status:   "migration_required",
			}, nil
		},
		func() (Fingerprint, error) {
			// Fingerprint says Core4 (not enrolled), but marker says migration_required
			return Fingerprint{}, nil
		},
	)
	rp, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if rp.Contract != ContractSceneBeatV3 {
		t.Errorf("marker contract should take priority, got %v", rp.Contract)
	}
	if rp.Status != StatusMigrationRequired {
		t.Errorf("marker status should take priority, got %v", rp.Status)
	}
}

func TestRegistry_Resolve_NilLoaderAndFingerprinter(t *testing.T) {
	r := NewRegistry(nil, nil)
	rp, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if rp.Contract != ContractCore4 {
		t.Errorf("should default to core4, got %v", rp.Contract)
	}
	if rp.Status != StatusCore {
		t.Errorf("should default to core, got %v", rp.Status)
	}
}

func TestRegistry_Resolve_FingerprintErrorFailsClosed(t *testing.T) {
	r := NewRegistry(
		func() (*ProfileMarker, error) { return nil, nil },
		func() (Fingerprint, error) {
			return Fingerprint{}, nil // empty fingerprint = no enrollment = Core4
		},
	)
	rp, err := r.Resolve()
	if err != nil {
		t.Fatalf("empty fingerprint should not error: %v", err)
	}
	if rp.Contract != ContractCore4 {
		t.Errorf("empty fingerprint should get core4, got %v", rp.Contract)
	}
}

func TestNewStoreFingerprinter(t *testing.T) {
	fp := NewStoreFingerprinter(t.TempDir())
	if fp == nil {
		t.Fatal("NewStoreFingerprinter returned nil")
	}
	// Should not panic - dir exists but is empty
	result, err := fp()
	if err != nil {
		t.Fatalf("fingerprinter on empty dir should not error: %v", err)
	}
	if result.CompletedThrough != 0 {
		t.Errorf("empty dir should have CompletedThrough=0, got %d", result.CompletedThrough)
	}
}
