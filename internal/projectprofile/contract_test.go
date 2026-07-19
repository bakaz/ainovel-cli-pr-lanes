package projectprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestContract_String(t *testing.T) {
	tests := []struct {
		c    Contract
		want string
	}{
		{ContractCore4, "core4"},
		{ContractSceneBeatV3, "scene_beat_v3"},
		{Contract(-1), "unknown(-1)"},
	}
	for _, tc := range tests {
		got := tc.c.String()
		if got != tc.want {
			t.Errorf("Contract(%d).String() = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestParseContract(t *testing.T) {
	tests := []struct {
		s       string
		want    Contract
		wantErr bool
	}{
		{"core4", ContractCore4, false},
		{"scene_beat_v3", ContractSceneBeatV3, false},
		{"unknown", ContractCore4, true},
		{"", ContractCore4, true},
	}
	for _, tc := range tests {
		got, err := ParseContract(tc.s)
		if tc.wantErr && err == nil {
			t.Errorf("ParseContract(%q) expected error", tc.s)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ParseContract(%q) unexpected error: %v", tc.s, err)
		}
		if got != tc.want {
			t.Errorf("ParseContract(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestSceneBeatV3Fields(t *testing.T) {
	// Verify v3 contract constructor copies fields from private source
	c := NewSceneBeatV3Contract()
	fields := c.RequiredFields()
	expected := []string{"goal", "action", "conflict", "outcome", "body_reaction", "emotion_reaction", "erotic_charge"}
	if len(fields) != len(expected) {
		t.Fatalf("v3 RequiredFields length = %d, want %d", len(fields), len(expected))
	}
	for i, f := range fields {
		if f != expected[i] {
			t.Errorf("v3 RequiredFields[%d] = %q, want %q", i, f, expected[i])
		}
	}
}

func TestNewFingerprint(t *testing.T) {
	premise := "测试前提"
	chapters := map[string]string{
		"01.md": "第一章内容",
		"02.md": "第二章内容",
	}
	fp := NewFingerprint(premise, chapters)

	// Verify premise hash
	ph := sha256.Sum256([]byte(premise))
	wantPH := hex.EncodeToString(ph[:])
	if fp.PremiseHash != wantPH {
		t.Errorf("PremiseHash = %q, want %q", fp.PremiseHash, wantPH)
	}

	// Verify chapters hash
	names := []string{"01.md", "02.md"}
	var chHashes []string
	for _, name := range names {
		h := sha256.Sum256([]byte(chapters[name]))
		chHashes = append(chHashes, hex.EncodeToString(h[:]))
	}
	joined := strings.Join(chHashes, "\x00")
	ch := sha256.Sum256([]byte(joined))
	wantCH := hex.EncodeToString(ch[:])
	if fp.ChaptersHash != wantCH {
		t.Errorf("ChaptersHash = %q, want %q", fp.ChaptersHash, wantCH)
	}

	if fp.CompletedThrough != 34 {
		t.Errorf("CompletedThrough = %d, want 34", fp.CompletedThrough)
	}
}

func TestFingerprint_IsV3Enrolled(t *testing.T) {
	// Test using known production fingerprint (immutable)
	prod := V3EnrolledFingerprint()
	if !prod.IsV3Enrolled() {
		t.Error("production fingerprint must be IsV3Enrolled")
	}

	// Different premise → not enrolled
	different := NewFingerprint("different premise", map[string]string{"01.md": "ch1"})
	if different.IsV3Enrolled() {
		t.Error("non-matching fingerprint should not be enrolled")
	}

	// Empty → not enrolled
	empty := Fingerprint{}
	if empty.IsV3Enrolled() {
		t.Error("empty fingerprint should not be enrolled")
	}
}

func TestV3EnrolledFingerprint(t *testing.T) {
	fp := V3EnrolledFingerprint()
	if fp.PremiseHash != v3PremiseHash {
		t.Errorf("PremiseHash = %q, want %q", fp.PremiseHash, v3PremiseHash)
	}
	if fp.ChaptersHash != v3ChaptersHash {
		t.Errorf("ChaptersHash = %q, want %q", fp.ChaptersHash, v3ChaptersHash)
	}
	if fp.CompletedThrough != 34 {
		t.Errorf("CompletedThrough = %d, want 34", fp.CompletedThrough)
	}
}
