package privacy

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestVersionAndEffectiveDateComeFromTheDocument is about the guarantee this
// issue really rests on: the version and the effective date exist in ONE
// place, the header of policy.md, parsed once at init. That guarantee is
// structural rather than asserted, and the honest reading of this test is that
// its first half cannot fail as long as the structure holds:
//
//   - the embedded document equals policy.md on disk because go:embed names
//     exactly that path; the assertion only bites if the directive is ever
//     pointed at another file, or the document becomes generated;
//   - Version()/EffectiveDate() equal a fresh parse of the file because
//     parseHeader is the only thing that produces them; the assertion bites
//     the day someone replaces the parse with a literal "1.0".
//
// What has teeth today is the second half: the answer the owner receives must
// carry the version and the effective date. That breaks if the header is
// reformatted, moved below the scan window, or dropped from the text the
// command sends -- none of which the structure prevents.
func TestVersionAndEffectiveDateComeFromTheDocument(t *testing.T) {
	onDisk, err := os.ReadFile("policy.md")
	if err != nil {
		t.Fatalf("reading policy.md: %v", err)
	}

	if string(onDisk) != document {
		t.Fatal("the embedded policy differs from policy.md on disk")
	}

	fileVersion, fileDate, err := parseHeader(string(onDisk))
	if err != nil {
		t.Fatalf("parsing the header of policy.md: %v", err)
	}
	if Version() != fileVersion {
		t.Fatalf("Version() = %q, policy.md says %q", Version(), fileVersion)
	}
	if EffectiveDate() != fileDate {
		t.Fatalf("EffectiveDate() = %q, policy.md says %q", EffectiveDate(), fileDate)
	}

	// The answer the owner receives carries them too: a user quoting their
	// copy and a reviewer quoting the repository must be able to tell they
	// are talking about the same policy.
	answer := Text()
	if !strings.Contains(answer, versionLabel+" "+Version()) {
		t.Fatalf("the answer does not carry %q %s", versionLabel, Version())
	}
	if !strings.Contains(answer, effectiveDateLabel+" "+EffectiveDate()) {
		t.Fatalf("the answer does not carry %q %s", effectiveDateLabel, EffectiveDate())
	}
}

// TestEffectiveDateIsAUsableDate rejects a version bumped without its date,
// or a date written in a format nobody can order.
func TestEffectiveDateIsAUsableDate(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version() is empty")
	}
	parsed, err := time.Parse(effectiveDateLayout, EffectiveDate())
	if err != nil {
		t.Fatalf("EffectiveDate() = %q, not %s: %v", EffectiveDate(), effectiveDateLayout, err)
	}
	if parsed.IsZero() {
		t.Fatal("EffectiveDate() is the zero date")
	}
}

// TestTextIsTheDocumentTrimmed: the answer is the document verbatim, not a
// summary of it. Only the surrounding whitespace differs.
func TestTextIsTheDocumentTrimmed(t *testing.T) {
	if Text() != strings.TrimSpace(document) {
		t.Fatal("Text() is not the embedded document")
	}
	if Text() == "" {
		t.Fatal("Text() is empty")
	}
	if strings.TrimSpace(Text()) != Text() {
		t.Fatal("Text() keeps surrounding whitespace")
	}
}

// TestPolicyCoversEveryRequiredTopic pins the acceptance criteria of the
// issue onto the document itself: each of these is a promise the product
// makes, and removing one from the text is a regression, not an edit.
func TestPolicyCoversEveryRequiredTopic(t *testing.T) {
	tests := []struct {
		topic   string
		needles []string
	}{
		{topic: "automatic saving without selection", needles: []string{"no chat picker", "no per-conversation opt-out"}},
		{topic: "exhaustive capture", needles: []string{"saved in full"}},
		{topic: "bot api limits", needles: []string{"Bot API", "no retroactive access", "20 MB"}},
		{topic: "what is stored", needles: []string{"caption", "attachments"}},
		{topic: "logs", needles: []string{"logs", "No message text"}},
		{topic: "retention", needles: []string{"retention_days", "1 and 365"}},
		{topic: "backup survival", needles: []string{"BACKUP_RETENTION_DAYS", "already written"}},
		{topic: "owner only", needles: []string{"only ever sent to the account holder", "third party asking for it receives nothing"}},
		{topic: "the command itself", needles: []string{"/privacy"}},
		// Where the command is typed is part of what the policy must disclose:
		// the Bot API only delivers business updates, so the command lands in a
		// monitored chat, in front of the contact, and is saved like any other
		// message. A reader who assumes a private chat with the bot would be
		// misled by the document, not by the code.
		{topic: "where the command is typed", needles: []string{
			"typed in a chat covered by",
			"saves it like any other message",
			"as a private message from the bot",
		}},
	}

	answer := Text()
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			for _, needle := range tt.needles {
				if !strings.Contains(answer, needle) {
					t.Fatalf("the policy never mentions %q", needle)
				}
			}
		})
	}
}

// TestPolicyNeverPromisesErasureFromBackups protects the one sentence that
// would become a lie later: content encryption (phase 4) turns archived rows
// into unreadable residue, it does not remove them from an archive already
// written. Any wording that promises their disappearance must fail here,
// today, rather than be discovered false after the fact.
func TestPolicyNeverPromisesErasureFromBackups(t *testing.T) {
	forbidden := []string{
		"deleted from the backups",
		"deleted from backups",
		"removed from the backups",
		"removed from backups",
		"erased from the backups",
		"erased from backups",
	}

	answer := strings.ToLower(Text())
	for _, claim := range forbidden {
		if strings.Contains(answer, claim) {
			t.Fatalf("the policy claims %q, which a deletion cannot deliver", claim)
		}
	}
}

// TestParseHeaderRejectsMalformedDocuments covers the failure modes init
// turns into a build-time panic.
func TestParseHeaderRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{name: "empty document", doc: ""},
		{name: "no version", doc: "Title\n\nEffective date: 2026-09-12\n"},
		{name: "no effective date", doc: "Title\n\nVersion: 1.0\n"},
		{name: "date is not a date", doc: "Title\n\nVersion: 1.0\nEffective date: soon\n"},
		{name: "date in another format", doc: "Title\n\nVersion: 1.0\nEffective date: 12/09/2026\n"},
		{name: "impossible day", doc: "Title\n\nVersion: 1.0\nEffective date: 2026-02-31\n"},
		{
			name: "header buried below the scan window",
			doc:  strings.Repeat("filler\n", headerScanLines) + "Version: 1.0\nEffective date: 2026-09-12\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseHeader(tt.doc); err == nil {
				t.Fatal("parseHeader accepted a malformed document")
			}
		})
	}
}

// TestParseHeaderAcceptsTheExpectedShape documents the exact shape required
// at the top of policy.md, and that the first occurrence wins: a later
// "Version:" inside a paragraph must not override the header.
func TestParseHeaderAcceptsTheExpectedShape(t *testing.T) {
	doc := "undelete — Privacy policy\n\nVersion: 2.1\nEffective date: 2027-01-31\n\nVersion: 9.9\n"
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
