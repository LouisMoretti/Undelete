package config

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// scripts/preflight.sh validates the QUOTA_* variables before a deploy so a
// value config.Load() would refuse is reported there rather than in a crash
// loop afterwards -- and a value preflight rejects while the runtime accepts
// blocks a deployment that would start fine. That only holds while the two
// agree, and they are written in different languages: this test runs BOTH on
// the same values and compares their verdicts, instead of restating the rule
// a third time (same pattern as TestPreflightAgreesWithTheAllowlistParser).
//
// The interesting halves are the canonical form ("007", "+5" and surrounding
// spaces: accepted by a bare ParseInt, refused by the canonical rule) and the
// range (a 23-digit run passes a digits-only shell case while ParseInt
// range-fails at boot).

func TestPreflightAgreesWithQuotaParser(t *testing.T) {
	script := preflightScript(t)

	quotaVars := []string{
		"QUOTA_MAX_MESSAGES_PER_TENANT",
		"QUOTA_MAX_MEDIA_FILES_PER_TENANT",
		"QUOTA_MAX_MEDIA_BYTES_PER_TENANT",
		"QUOTA_CAPTURES_PER_MINUTE_PER_TENANT",
	}

	values := []struct {
		name  string
		value string
	}{
		{name: "empty keeps default", value: ""},
		{name: "plain", value: "1000"},
		{name: "one", value: "1"},
		{name: "max int64", value: "9223372036854775807"},
		{name: "max int64 plus one", value: "9223372036854775808"},
		{name: "nineteen digits above max", value: "9999999999999999999"},
		{name: "twenty-three digits", value: "99999999999999999999999"},
		{name: "leading zero", value: "007"},
		{name: "zero", value: "0"},
		{name: "zero padded", value: "00"},
		{name: "plus sign", value: "+5"},
		{name: "leading space", value: " 300"},
		{name: "trailing space", value: "300 "},
		{name: "negative", value: "-5"},
		{name: "not a number", value: "a lot"},
		{name: "suffixed unit", value: "5GiB"},
		{name: "float", value: "12.5"},
		{name: "inner space", value: "3 00"},
	}

	for _, env := range quotaVars {
		for _, tc := range values {
			t.Run(env+"/"+tc.name, func(t *testing.T) {
				_, wantErr := parseCanonicalPositiveInt64(strings.TrimSpace(tc.value))
				wantValid := tc.value == "" || strings.TrimSpace(tc.value) == "" || wantErr == nil
				// An all-whitespace value trims to empty: Load keeps the
				// default (valid), while preflight sees an empty trimmed
				// value and must also accept. Neither side is exercised
				// with such a value in production; treat empty-trimmed as
				// valid on both sides.
				if strings.TrimSpace(tc.value) == "" {
					wantValid = true
				}
				gotValid := runPreflightQuotaCheck(t, script, env, tc.value)
				if gotValid != wantValid {
					if wantValid {
						t.Fatalf("preflight refuses %s=%q that config.Load() accepts", env, tc.value)
					}
					t.Fatalf("preflight accepts %s=%q that config.Load() refuses", env, tc.value)
				}
			})
		}
	}
}

func TestPreflightAgreesWithWarnPercentParser(t *testing.T) {
	script := preflightScript(t)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty keeps default", value: ""},
		{name: "low", value: "1"},
		{name: "typical", value: "80"},
		{name: "high", value: "99"},
		{name: "zero", value: "0"},
		{name: "hundred", value: "100"},
		{name: "three digits", value: "1000"},
		{name: "leading zero", value: "007"},
		{name: "plus sign", value: "+5"},
		{name: "leading space", value: " 80"},
		{name: "trailing space", value: "80 "},
		{name: "negative", value: "-5"},
		{name: "not a number", value: "eighty"},
		{name: "float", value: "8.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trimmed := strings.TrimSpace(tc.value)
			wantValid := tc.value == "" || trimmed == ""
			if !wantValid {
				_, err := parseCanonicalWarnPercent(trimmed)
				wantValid = err == nil
			}
			gotValid := runPreflightQuotaCheck(t, script, "QUOTA_WARN_PERCENT", tc.value)
			if gotValid != wantValid {
				if wantValid {
					t.Fatalf("preflight refuses QUOTA_WARN_PERCENT=%q that config.Load() accepts", tc.value)
				}
				t.Fatalf("preflight accepts QUOTA_WARN_PERCENT=%q that config.Load() refuses", tc.value)
			}
		})
	}
}

// runPreflightQuotaCheck runs the whole preflight with one value for one
// quota variable and reads back the verdict of that single check. The script
// exits 1 as soon as anything else is missing (no DSN, no token), which is
// expected and irrelevant here: what is read is the quota line, never the
// exit code.
func runPreflightQuotaCheck(t *testing.T, script, env, value string) bool {
	t.Helper()

	cmd := exec.Command("sh", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		env + "=" + value,
	}
	output, err := cmd.CombinedOutput()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		t.Fatalf("running %s: %v\n%s", script, err, output)
	}

	sawLine := false
	valid := false
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "[FAIL] "+env) {
			sawLine = true
			valid = false
		} else if strings.HasPrefix(line, "[ OK ] "+env+"=") || strings.HasPrefix(line, "[ OK ] "+env+" not set") {
			sawLine = true
			valid = true
		}
	}
	if !sawLine {
		t.Fatalf("preflight reported nothing about %s=%q:\n%s", env, value, output)
	}
	return valid
}
