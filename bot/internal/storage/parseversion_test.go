package storage

import (
	"testing"
)

// TestParseVersionCoversMigrationNames pins the ledger ordering: the numeric
// prefix drives application order, and malformed names must fail loudly
// rather than sort somewhere surprising.
func TestParseVersionCoversMigrationNames(t *testing.T) {
	tests := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{name: "0001_init.sql", want: 1},
		{name: "0004_media_files.sql", want: 4},
		{name: "10_late.sql", want: 10},
		{name: "init.sql", wantErr: true},
		{name: "nodigits_name.sql", wantErr: true},
		{name: ".sql", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVersion(tt.name)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseVersion(%q) = %d, want an error", tt.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVersion(%q): %v", tt.name, err)
			}
			if got != tt.want {
				t.Fatalf("parseVersion(%q) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}
