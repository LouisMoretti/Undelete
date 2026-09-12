// Package privacy owns the privacy policy served by the /privacy command.
//
// The policy has ONE source of truth: policy.md, versioned in the repository
// and embedded in the binary. The command does not paraphrase it and does not
// hold its own copy of the version or of the effective date -- both are read
// back from the embedded document at startup, so the answer a user receives
// and the document reviewers read can never describe two different policies.
package privacy

import (
	_ "embed"
	"fmt"
	"strings"
	"time"
)

// document is the policy as served. Embedding (rather than reading the file
// at runtime) keeps the answer available on a container that ships the binary
// alone, and makes the document part of what the build and the tests check.
//
//go:embed policy.md
var document string

// Header field labels, as written at the top of policy.md.
const (
	versionLabel       = "Version:"
	effectiveDateLabel = "Effective date:"
)

// effectiveDateLayout is the effective-date format: a plain calendar day, no
// timezone. The policy applies to a date, not to an instant.
const effectiveDateLayout = "2006-01-02"

// headerScanLines bounds how far into the document the header is looked for.
// The header sits in the first few lines; scanning the whole document would
// let a "Version:" written in a later paragraph pass for it.
const headerScanLines = 10

var (
	version       string
	effectiveDate string
)

// init resolves the version and the effective date from the embedded
// document. A malformed header is a defect in a file that is compiled into
// the binary, not a runtime condition: failing here surfaces it on the first
// build and on every test run, rather than shipping a policy nobody can date.
func init() {
	var err error
	version, effectiveDate, err = parseHeader(document)
	if err != nil {
		panic(fmt.Sprintf("privacy: invalid policy.md header: %v", err))
	}
}

// Version is the policy version, as written in policy.md.
func Version() string { return version }

// EffectiveDate is the date the policy came into force, as written in
// policy.md, formatted YYYY-MM-DD.
func EffectiveDate() string { return effectiveDate }

// Text is the answer of the /privacy command: the document itself, verbatim.
// Trailing whitespace is trimmed because Telegram rejects a message that is
// only whitespace and keeps the trailing newline visible.
func Text() string { return strings.TrimSpace(document) }

// parseHeader extracts the version and the effective date from the first
// lines of the document. It returns an error rather than a zero value: an
// undated policy is worse than no answer at all.
func parseHeader(doc string) (string, string, error) {
	lines := strings.Split(doc, "\n")
	if len(lines) > headerScanLines {
		lines = lines[:headerScanLines]
	}

	var parsedVersion, parsedDate string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, versionLabel) && parsedVersion == "":
			parsedVersion = strings.TrimSpace(strings.TrimPrefix(line, versionLabel))
		case strings.HasPrefix(line, effectiveDateLabel) && parsedDate == "":
			parsedDate = strings.TrimSpace(strings.TrimPrefix(line, effectiveDateLabel))
		}
	}

	if parsedVersion == "" {
		return "", "", fmt.Errorf("missing %q line in the first %d lines", versionLabel, headerScanLines)
	}
	if parsedDate == "" {
		return "", "", fmt.Errorf("missing %q line in the first %d lines", effectiveDateLabel, headerScanLines)
	}
	if _, err := time.Parse(effectiveDateLayout, parsedDate); err != nil {
		return "", "", fmt.Errorf("effective date %q is not %s", parsedDate, effectiveDateLayout)
	}
	return parsedVersion, parsedDate, nil
}
