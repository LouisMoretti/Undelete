package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scripts/preflight.sh validates OWNER_ALLOWLIST_TELEGRAM_USER_IDS before a
// deploy so a value config.Load() would refuse is reported here rather than in
// a crash loop afterwards. That only holds while the two agree, and they are
// written in different languages: this test runs BOTH on the same values and
// compares their verdicts, instead of restating the rule a third time.
//
// The interesting half is the range. A Telegram user id is parsed by
// strconv.ParseInt into an int64, so 9223372036854775808 is refused at boot
// while being, to a digits-only shell `case`, a perfectly good positive
// integer.

// restrictedLine matches the accepting line of the preflight check, from which
// the number of admitted holders is read back.
var restrictedLine = regexp.MustCompile(`^\[ OK \] OWNER_ALLOWLIST_TELEGRAM_USER_IDS: onboarding restricted to (\d+) account holder\(s\)$`)

func TestPreflightAgreesWithTheAllowlistParser(t *testing.T) {
	script := preflightScript(t)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty is open onboarding", value: ""},
		{name: "one id", value: "123456789"},
		{name: "comma separated", value: "123,456"},
		{name: "space separated", value: "123 456 789"},
		{name: "both separators", value: "1, 2,3"},
		{name: "max int64", value: "9223372036854775807"},
		{name: "max int64 plus one", value: "9223372036854775808"},
		{name: "nineteen digits above max", value: "9999999999999999999"},
		{name: "another nineteen digits above max", value: "9300000000000000000"},
		{name: "twenty digits", value: "99999999999999999999"},
		{name: "thirty-five digits", value: "12345678901234567890123456789012345"},
		{name: "overflow among valid entries", value: "123,99999999999999999999,456"},
		{name: "leading zero", value: "0755"},
		{name: "zero", value: "0"},
		{name: "negative", value: "-5"},
		{name: "not a number", value: "abc"},
		{name: "float", value: "12.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantList, wantErr := parseOwnerAllowlist(tc.value)
			gotValid, gotCount, gotOpen := runPreflightAllowlistCheck(t, script, tc.value)

			switch {
			case wantErr != nil:
				if gotValid {
					t.Fatalf("preflight accepts %q that config.Load() refuses (%v): a deployment would pass preflight and crash-loop", tc.value, wantErr)
				}
			case len(wantList) == 0:
				if !gotOpen {
					t.Fatalf("preflight did not report OPEN ONBOARDING for %q, which the parser reads as an empty allowlist", tc.value)
				}
			default:
				if !gotValid {
					t.Fatalf("preflight refuses %q that config.Load() accepts as %v", tc.value, wantList)
				}
				if gotCount != len(wantList) {
					t.Fatalf("preflight reports %d admitted holder(s) for %q, the parser reads %d (%v)", gotCount, tc.value, len(wantList), wantList)
				}
			}
		})
	}
}

// preflightScript locates scripts/preflight.sh from the module and skips the
// test where it cannot be run honestly.
func preflightScript(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	script := filepath.Join(root, "scripts", "preflight.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("scripts/preflight.sh not found at %s: %v", script, err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no POSIX shell available: %v", err)
	}
	// preflight.sh loads the repository's .env when there is one, which would
	// silently feed the run values this test did not choose (and could trigger
	// the getMe call). A working copy that has one is not a place this
	// comparison can be trusted from.
	if _, err := os.Stat(filepath.Join(root, ".env")); err == nil {
		t.Skip("a local .env would be loaded by preflight.sh: run this check on a clean checkout")
	}
	return script
}

// runPreflightAllowlistCheck runs the whole preflight with one value for the
// allowlist and reads back the verdict of that single check. The script exits 1
// as soon as anything else is missing (no DSN, no token), which is expected and
// irrelevant here: what is read is the allowlist line, never the exit code.
func runPreflightAllowlistCheck(t *testing.T, script, value string) (valid bool, count int, open bool) {
	t.Helper()

	cmd := exec.Command("sh", script)
	// A deliberately minimal environment: every other variable absent means
	// every other check reports missing or skipped, and none of them reaches
	// the network or a database.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"OWNER_ALLOWLIST_TELEGRAM_USER_IDS=" + value,
	}
	output, err := cmd.CombinedOutput()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		t.Fatalf("running %s: %v\n%s", script, err, output)
	}

	sawLine := false
	for _, line := range strings.Split(string(output), "\n") {
		switch {
		case strings.HasPrefix(line, "[FAIL] OWNER_ALLOWLIST_TELEGRAM_USER_IDS"):
			sawLine = true
			valid = false
			return valid, 0, false
		case strings.HasPrefix(line, "[ OK ] OWNER_ALLOWLIST_TELEGRAM_USER_IDS empty:"):
			sawLine = true
			valid, open = true, true
		default:
			if match := restrictedLine.FindStringSubmatch(line); match != nil {
				sawLine = true
				valid = true
				count = atoi(t, match[1])
			}
		}
	}
	if !sawLine {
		t.Fatalf("preflight reported nothing about OWNER_ALLOWLIST_TELEGRAM_USER_IDS=%q:\n%s", value, output)
	}
	return valid, count, open
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
