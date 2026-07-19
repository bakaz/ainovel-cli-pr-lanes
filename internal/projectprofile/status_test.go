package projectprofile

import "testing"

func TestStatus_String(t *testing.T) {
	tests := []struct {
		s    Status
		want string
	}{
		{StatusCore, "core"},
		{StatusMigrationRequired, "migration_required"},
		{StatusActive, "active"},
		{Status(-1), "status(-1)"},
	}
	for _, tc := range tests {
		got := tc.s.String()
		if got != tc.want {
			t.Errorf("Status(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		s       string
		want    Status
		wantErr bool
	}{
		{"core", StatusCore, false},
		{"migration_required", StatusMigrationRequired, false},
		{"active", StatusActive, false},
		{"unknown", StatusCore, true},
		{"", StatusCore, true},
	}
	for _, tc := range tests {
		got, err := ParseStatus(tc.s)
		if tc.wantErr && err == nil {
			t.Errorf("ParseStatus(%q) expected error", tc.s)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ParseStatus(%q) unexpected error: %v", tc.s, err)
		}
		if got != tc.want {
			t.Errorf("ParseStatus(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
