package privacy

import (
	"strings"
	"testing"
)

// TestParseHeaderTrimsSurroundingWhitespace pins the TrimSpace around both
// header values: a version bumped by an editor that leaves trailing spaces
// must still read back as the version the answer carries.
func TestParseHeaderTrimsSurroundingWhitespace(t *testing.T) {
	doc := "undelete — Privacy policy\n\nVersion:   2.1  \nEffective date:   2027-01-31  \n"
	version, date, err := parseHeader(doc)
	if err != nil {
		t.Fatalf("parseHeader: %v", err)
	}
	if version != "2.1" {
		t.Fatalf("version = %q, want %q", version, "2.1")
	}
	if date != "2027-01-31" {
		t.Fatalf("effective date = %q, want %q", date, "2027-01-31")
	}

	crlf := "undelete — Privacy policy\r\n\r\nVersion: 2.1\r\nEffective date: 2027-01-31\r\n"
	version, date, err = parseHeader(crlf)
	if err != nil {
		t.Fatalf("parseHeader with CRLF: %v", err)
	}
	if version != "2.1" || date != "2027-01-31" {
		t.Fatalf("CRLF parse = (%q, %q), want (%q, %q)", version, date, "2.1", "2027-01-31")
	}
}

// TestParseHeaderFirstEffectiveDateWins is the mirror of the version
// first-wins case in TestParseHeaderAcceptsTheExpectedShape: a later
// "Effective date:" inside a paragraph must not override the header.
func TestParseHeaderFirstEffectiveDateWins(t *testing.T) {
	doc := "undelete — Privacy policy\n\nVersion: 1.0\nEffective date: 2027-01-31\n\nEffective date: 1999-12-31\n"
	_, date, err := parseHeader(doc)
	if err != nil {
		t.Fatalf("parseHeader: %v", err)
	}
	if date != "2027-01-31" {
		t.Fatalf("effective date = %q, want first occurrence %q", date, "2027-01-31")
	}
}

// TestParseHeaderAcceptsHeaderOnLastScannedLine pins the scan-window edge:
// a header sitting exactly on the last scanned line is still found, while a
// header one line further down is not (covered by the buried-header case).
// This guards an off-by-one on headerScanLines in either direction.
func TestParseHeaderAcceptsHeaderOnLastScannedLine(t *testing.T) {
	doc := strings.Repeat("filler\n", headerScanLines-2) + "Version: 1.0\nEffective date: 2026-09-12\n"
	version, date, err := parseHeader(doc)
	if err != nil {
		t.Fatalf("parseHeader with header on the last scanned line: %v", err)
	}
	if version != "1.0" || date != "2026-09-12" {
		t.Fatalf("parse = (%q, %q), want (%q, %q)", version, date, "1.0", "2026-09-12")
	}
}

// TestParseHeaderAcceptsFieldsInAnyOrder documents that the header is found
// by label, not by line number: swapping the two lines still parses.
func TestParseHeaderAcceptsFieldsInAnyOrder(t *testing.T) {
	doc := "undelete — Privacy policy\n\nEffective date: 2027-01-31\nVersion: 2.1\n"
	version, date, err := parseHeader(doc)
	if err != nil {
		t.Fatalf("parseHeader: %v", err)
	}
	if version != "2.1" {
		t.Fatalf("version = %q, want %q", version, "2.1")
	}
	if date != "2027-01-31" {
		t.Fatalf("effective date = %q, want %q", date, "2027-01-31")
	}
}

// TestParseHeaderRejectsIndentedAndMiscasedLabels pins the exact shape the
// header requires: labels start at column zero with the documented casing.
// A reformatted header must fail loudly at init, not serve an undated policy.
func TestParseHeaderRejectsIndentedAndMiscasedLabels(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{name: "indented version", doc: "Title\n\n Version: 1.0\nEffective date: 2026-09-12\n"},
		{name: "indented date", doc: "Title\n\nVersion: 1.0\n Effective date: 2026-09-12\n"},
		{name: "lowercase version", doc: "Title\n\nversion: 1.0\nEffective date: 2026-09-12\n"},
		{name: "lowercase date", doc: "Title\n\nVersion: 1.0\neffective date: 2026-09-12\n"},
		{name: "uppercase version", doc: "Title\n\nVERSION: 1.0\nEffective date: 2026-09-12\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseHeader(tt.doc); err == nil {
				t.Fatal("parseHeader accepted a misformatted label")
			}
		})
	}
}

// TestParseHeaderRejectsEmptyValues: a label with nothing after the colon is
// not a version or a date. An undated policy is worse than no answer, so it
// must fail the same way a missing line does.
func TestParseHeaderRejectsEmptyValues(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{name: "empty version", doc: "Title\n\nVersion:\nEffective date: 2026-09-12\n"},
		{name: "blank version", doc: "Title\n\nVersion:   \nEffective date: 2026-09-12\n"},
		{name: "empty date", doc: "Title\n\nVersion: 1.0\nEffective date:\n"},
		{name: "blank date", doc: "Title\n\nVersion: 1.0\nEffective date:   \n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseHeader(tt.doc); err == nil {
				t.Fatal("parseHeader accepted an empty header value")
			}
		})
	}
}

// TestParseHeaderErrorNamesTheMissingField keeps the init panic actionable:
// the error must say which label is missing so the fix is obvious.
func TestParseHeaderErrorNamesTheMissingField(t *testing.T) {
	_, _, err := parseHeader("Title\n\nEffective date: 2026-09-12\n")
	if err == nil {
		t.Fatal("parseHeader accepted a document without a version")
	}
	if !strings.Contains(err.Error(), versionLabel) {
		t.Fatalf("error %q does not name %q", err, versionLabel)
	}

	_, _, err = parseHeader("Title\n\nVersion: 1.0\n")
	if err == nil {
		t.Fatal("parseHeader accepted a document without an effective date")
	}
	if !strings.Contains(err.Error(), effectiveDateLabel) {
		t.Fatalf("error %q does not name %q", err, effectiveDateLabel)
	}
}

// TestLiveHeaderValuesCarryNoPadding: the values init resolved from the
// embedded document must already be trimmed, since the answer check compares
// label+" "+value verbatim.
func TestLiveHeaderValuesCarryNoPadding(t *testing.T) {
	if Version() != strings.TrimSpace(Version()) {
		t.Fatalf("Version() = %q carries surrounding whitespace", Version())
	}
	if EffectiveDate() != strings.TrimSpace(EffectiveDate()) {
		t.Fatalf("EffectiveDate() = %q carries surrounding whitespace", EffectiveDate())
	}
}
