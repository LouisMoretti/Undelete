package config

import (
	"strings"
	"testing"
)

func TestParseCanonicalPositiveInt64(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{"valid small", "1", 1, false, ""},
		{"valid medium", "1000", 1000, false, ""},
		{"valid max int64", "9223372036854775807", 9223372036854775807, false, ""},

		// isCanonicalPositiveDecimal rejects (line 331)
		{"empty", "", 0, true, "canonical decimal form"},
		{"zero", "0", 0, true, "canonical decimal form"},
		{"leading zero", "007", 0, true, "canonical decimal form"},
		{"plus sign", "+5", 0, true, "canonical decimal form"},
		{"negative", "-5", 0, true, "canonical decimal form"},
		{"float", "12.5", 0, true, "canonical decimal form"},
		{"non-digits", "abc", 0, true, "canonical decimal form"},
		{"inner space", "3 00", 0, true, "canonical decimal form"},

		// strconv.ParseInt overflow (line 335)
		{"overflow 19 digits", "9999999999999999999", 0, true, "fits in a signed 64-bit integer"},
		{"overflow 23 digits", "99999999999999999999999", 0, true, "fits in a signed 64-bit integer"},
		{"overflow max int64 + 1", "9223372036854775808", 0, true, "fits in a signed 64-bit integer"},

		// value <= 0 after parse (line 338) - theoretically unreachable but defensive
		// Note: isCanonicalPositiveDecimal already rejects "0" and leading zeros,
		// so this branch is defensive. We test it's not reached for valid inputs.
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCanonicalPositiveInt64(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCanonicalPositiveInt64(%q) = %d, want error", tc.input, got)
				}
				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("parseCanonicalPositiveInt64(%q) error = %q, want to contain %q", tc.input, err.Error(), tc.errMsg)
				}
			} else {
				if err != nil {
					t.Fatalf("parseCanonicalPositiveInt64(%q) unexpected error: %v", tc.input, err)
				}
				if got != tc.want {
					t.Fatalf("parseCanonicalPositiveInt64(%q) = %d, want %d", tc.input, got, tc.want)
				}
			}
		})
	}
}

func TestParseCanonicalWarnPercent(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
		errMsg  string
	}{
		// Valid cases (1-99)
		{"valid low", "1", 1, false, ""},
		{"valid typical", "80", 80, false, ""},
		{"valid high", "99", 99, false, ""},

		// isCanonicalPositiveDecimal rejects (line 347)
		{"empty", "", 0, true, "canonical decimal form"},
		{"zero", "0", 0, true, "canonical decimal form"},
		{"leading zero", "007", 0, true, "canonical decimal form"},
		{"plus sign", "+5", 0, true, "canonical decimal form"},
		{"negative", "-5", 0, true, "canonical decimal form"},
		{"float", "8.5", 0, true, "canonical decimal form"},
		{"non-digits", "eighty", 0, true, "canonical decimal form"},

		// strconv.Atoi overflow (line 351) - very large number
		{"overflow", "99999999999999999999999999999999999999", 0, true, "value out of range"},

		// percent <= 0 || percent >= 100 (line 354): canonical digits that
		// pass isCanonicalPositiveDecimal but fail the 1-99 range check.
		// "0" itself is caught earlier by isCanonicalPositiveDecimal.
		{"hundred", "100", 0, true, "expected between 1 and 99"},
		{"large number", "1000", 0, true, "expected between 1 and 99"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCanonicalWarnPercent(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCanonicalWarnPercent(%q) = %d, want error", tc.input, got)
				}
				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("parseCanonicalWarnPercent(%q) error = %q, want to contain %q", tc.input, err.Error(), tc.errMsg)
				}
			} else {
				if err != nil {
					t.Fatalf("parseCanonicalWarnPercent(%q) unexpected error: %v", tc.input, err)
				}
				if got != tc.want {
					t.Fatalf("parseCanonicalWarnPercent(%q) = %d, want %d", tc.input, got, tc.want)
				}
			}
		})
	}
}
