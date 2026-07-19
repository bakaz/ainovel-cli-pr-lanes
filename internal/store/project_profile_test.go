package store

import (
	"strings"
	"testing"
)

func TestProjectProfileStore_LoadRaw_FileNotFound(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	raw, err := s.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw on empty dir should succeed: %v", err)
	}
	if raw != nil {
		t.Fatal("LoadRaw should return nil when file doesn't exist")
	}
}

func TestProjectProfileStore_SaveAndLoad(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))

	marker := ProfileData{
		Version:  "v1",
		Contract: "scene_beat_v3",
		Status:   "migration_required",
	}
	if err := s.SaveRaw(marker); err != nil {
		t.Fatalf("SaveRaw failed: %v", err)
	}

	loaded, err := s.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadRaw returned nil after save")
	}
	if loaded.Version != "v1" {
		t.Errorf("Version = %q, want v1", loaded.Version)
	}
	if loaded.Contract != "scene_beat_v3" {
		t.Errorf("Contract = %q, want scene_beat_v3", loaded.Contract)
	}
	if loaded.Status != "migration_required" {
		t.Errorf("Status = %q, want migration_required", loaded.Status)
	}
}

func TestProjectProfileStore_LoadRaw_EmptyFile(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	// Create empty file by writing an empty JSON object with no fields
	if err := s.io.WriteJSONUnlocked("meta/project_profile.json", map[string]string{}); err != nil {
		t.Fatalf("write empty: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil {
		t.Fatal("empty file should fail")
	}
}

func TestProjectProfileStore_LoadRaw_MissingVersion(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if err := s.io.WriteJSONUnlocked("meta/project_profile.json", ProfileData{
		Version:  "",
		Contract: "core4",
		Status:   "core",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil {
		t.Fatal("missing version should fail")
	}
}

func TestProjectProfileStore_LoadRaw_InvalidJSON(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte(`{invalid}`)); err != nil {
		t.Fatalf("write invalid: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil {
		t.Fatal("invalid JSON should fail")
	}
}

func TestProjectProfileStore_LoadRaw_ZeroBytes(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte{}); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil || !strings.Contains(err.Error(), "zero bytes") {
		t.Fatalf("zero-byte file should fail, got: %v", err)
	}
}

func TestProjectProfileStore_LoadRaw_TrailingData(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	// 写入第一个完整 JSON 对象后跟第二个文档
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}{"extra":"doc"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil || !strings.Contains(err.Error(), "trailing JSON document") {
		t.Fatalf("trailing JSON document should fail, got: %v", err)
	}
}

func TestProjectProfileStore_LoadRaw_UnknownFieldsRejected(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	// Write JSON with unknown field
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte(`{"version":"v1","contract":"core4","status":"core","unknown_field":"should_fail"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil {
		t.Fatal("unknown fields should be rejected by DisallowUnknownFields")
	}
}

func TestProjectProfileStore_Exists(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if s.Exists() {
		t.Error("Exists should be false before save")
	}

	_ = s.SaveRaw(ProfileData{Version: "v1", Contract: "core4", Status: "core"})
	if !s.Exists() {
		t.Error("Exists should be true after save")
	}
}

func TestProjectProfileStore_SaveAndLoadCore4(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))

	marker := ProfileData{
		Version:  "v1",
		Contract: "core4",
		Status:   "core",
	}
	if err := s.SaveRaw(marker); err != nil {
		t.Fatalf("SaveRaw failed: %v", err)
	}

	loaded, err := s.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw failed: %v", err)
	}
	if loaded.Contract != "core4" || loaded.Status != "core" {
		t.Errorf("got contract=%q status=%q", loaded.Contract, loaded.Status)
	}
}

func TestProjectProfileStore_LoadRaw_TrailingGarbage_Rejected(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}garbage`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil || !strings.Contains(err.Error(), "unexpected content") {
		t.Fatalf("trailing garbage should fail, got: %v", err)
	}
}

func TestProjectProfileStore_LoadRaw_TrailingUnmatchedBracket_Rejected(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}]`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil || !strings.Contains(err.Error(), "unexpected content") {
		t.Fatalf("trailing unmatched bracket should fail, got: %v", err)
	}
}

func TestProjectProfileStore_LoadRaw_TrailingUnmatchedCloseBrace_Rejected(t *testing.T) {
	s := NewProjectProfileStore(newIO(t.TempDir()))
	if err := s.io.WriteFileUnlocked("meta/project_profile.json", []byte(`{"version":"v1","contract":"scene_beat_v3","status":"migration_required"}}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := s.LoadRaw()
	if err == nil || !strings.Contains(err.Error(), "unexpected content") {
		t.Fatalf("trailing extra brace should fail, got: %v", err)
	}
}
