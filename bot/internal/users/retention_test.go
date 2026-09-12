package users

import (
	"testing"
)

// TestParseRetentionDays pins the bounds of /retention: 1 and 365 are
// accepted, everything else -- out of bounds, non-numeric, empty, or trailed
// by extra tokens -- is refused and the caller must change nothing.
func TestParseRetentionDays(t *testing.T) {
	valid := []struct {
		argument string
		want     int
	}{
		{argument: "1", want: 1},
		{argument: "7", want: 7},
		{argument: "365", want: 365},
		{argument: "  30  ", want: 30},
	}
	for _, tt := range valid {
		t.Run("accept "+tt.argument, func(t *testing.T) {
			got, err := ParseRetentionDays(tt.argument)
			if err != nil {
				t.Fatalf("ParseRetentionDays(%q) = (_, %v), want (_, nil)", tt.argument, err)
			}
			if got != tt.want {
				t.Fatalf("ParseRetentionDays(%q) = %d, want %d", tt.argument, got, tt.want)
			}
		})
	}

	invalid := []string{
		"",
		"   ",
		"0",
		"366",
		"1000",
		"-1",
		"-365",
		"abc",
		"3.5",
		"30 days",
		"10 20",
		"0x1E",
		"30,",
		"thirty",
		"007",
		"007 ",
		"+7",
		"00",
	}
	for _, argument := range invalid {
		t.Run("refuse "+argument, func(t *testing.T) {
			if days, err := ParseRetentionDays(argument); err == nil {
				t.Fatalf("ParseRetentionDays(%q) = %d, want an error", argument, days)
			}
		})
	}
}

// TestRetentionBoundsMirrorTheSchema pins the constants against the CHECK
// constraint of migration 0001: the Go validation and the database must agree
// on what a valid period is, otherwise the backstop and the gate diverge.
func TestRetentionBoundsMirrorTheSchema(t *testing.T) {
	if MinRetentionDays != 1 {
		t.Fatalf("MinRetentionDays = %d, want 1 (migration 0001 CHECK)", MinRetentionDays)
	}
	if MaxRetentionDays != 365 {
		t.Fatalf("MaxRetentionDays = %d, want 365 (migration 0001 CHECK)", MaxRetentionDays)
	}
	if DefaultRetentionDays != 7 {
		t.Fatalf("DefaultRetentionDays = %d, want 7 (migration 0001 DEFAULT)", DefaultRetentionDays)
	}
}
