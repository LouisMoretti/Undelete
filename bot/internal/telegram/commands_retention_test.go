package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// TestRetentionIsParsedLikeAnyCommand: the retention command goes through the
// same reader as /privacy, so the rules that protect one protect the other.
// The entry worth reading twice is "/retention_extra": an underscore is part
// of a command word, so that is a DIFFERENT command, never this one with a
// suffix.
func TestRetentionIsParsedLikeAnyCommand(t *testing.T) {
	tests := []struct {
		text     string
		command  string
		argument string
	}{
		{text: "/retention", command: CommandRetention},
		{text: "/Retention", command: CommandRetention},
		{text: "/RETENTION", command: CommandRetention},
		{text: "/retention@undelete_bot", command: CommandRetention},
		{text: "/retention 30", command: CommandRetention, argument: "30"},
		{text: "/retention@undelete_bot 30", command: CommandRetention, argument: "30"},
		{text: "/retention\r\n30", command: CommandRetention, argument: "30"},
		{text: "/retention   30  ", command: CommandRetention, argument: "30"},
		// Different command words: they must not reach the retention store.
		{text: "/retention_extra", command: "/retention_extra"},
		{text: "/retention_extra 30", command: "/retention_extra", argument: "30"},
		{text: "/retentio", command: "/retentio"},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			command, ok := ParseCommand(tt.text)
			if !ok {
				t.Fatalf("ParseCommand(%q) reported no command", tt.text)
			}
			if command != tt.command {
				t.Fatalf("command = %q, want %q", command, tt.command)
			}
			if argument := CommandArgument(tt.text); argument != tt.argument {
				t.Fatalf("argument = %q, want %q", argument, tt.argument)
			}
		})
	}
}

// retentionRequests is every message the /retention command can send, built
// the way production builds them.
func retentionRequests() map[string]SendMessageRequest {
	const owner = int64(700001)
	return map[string]SendMessageRequest{
		"status":  BuildRetentionStatusRequest(owner, 30, 14),
		"changed": BuildRetentionChangedRequest(owner, 30, 14),
		"usage":   BuildRetentionUsageRequest(owner),
	}
}

// TestRetentionMessagesGoToTheOwnerAndFitTheLimit covers the wire contract
// shared by all of them: one message, addressed to the owner's own Telegram
// id, never empty, and within the Bot API limit.
func TestRetentionMessagesGoToTheOwnerAndFitTheLimit(t *testing.T) {
	for name, req := range retentionRequests() {
		t.Run(name, func(t *testing.T) {
			if req.ChatID != 700001 {
				t.Fatalf("chat_id = %d, want the owner (700001)", req.ChatID)
			}
			if strings.TrimSpace(req.Text) == "" {
				t.Fatal("empty message: Telegram would refuse it")
			}
			if units := len(utf16.Encode([]rune(req.Text))); units > telegramTextLimit {
				t.Fatalf("message is %d UTF-16 units, over the %d limit", units, telegramTextLimit)
			}
		})
	}
}

// TestRetentionAnswersStateValuePurgeAndBackups is the acceptance criterion of
// the issue: the read and the confirmation state the current value, when the
// daily purge applies it, and the independence from the backups -- with the
// configured BACKUP_RETENTION_DAYS as a number, default 14.
func TestRetentionAnswersStateValuePurgeAndBackups(t *testing.T) {
	for _, days := range []int{7, 14, 90} {
		for name, text := range map[string]string{
			"status":  BuildRetentionStatusRequest(700001, 30, days).Text,
			"changed": BuildRetentionChangedRequest(700001, 30, days).Text,
		} {
			t.Run(name, func(t *testing.T) {
				for _, needle := range []string{
					"30 days",
					"24 hours",
					"measured from the date each message was received",
					"BACKUP_RETENTION_DAYS",
					"not purged automatically",
					"currently " + itoa(days) + " days",
					"does not rewrite an archive that was already written",
				} {
					if !strings.Contains(text, needle) {
						t.Fatalf("with %d backup days, the message never mentions %q", days, needle)
					}
				}
			})
		}
	}
}

// TestRetentionAnswersRenderOneDaySingular: the unit is part of the answer,
// and "1 days" would read as a defect in the one case the owner is most
// likely to set deliberately.
func TestRetentionAnswersRenderOneDaySingular(t *testing.T) {
	for name, text := range map[string]string{
		"status":  BuildRetentionStatusRequest(700001, 1, 14).Text,
		"changed": BuildRetentionChangedRequest(700001, 1, 14).Text,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(text, "1 day") {
				t.Fatalf("the message never mentions %q:\n%s", "1 day", text)
			}
			if strings.Contains(text, "1 days") {
				t.Fatalf("the message says %q:\n%s", "1 days", text)
			}
		})
	}
}

// TestRetentionMessagesNeverPromiseErasureFromBackups is the same guard the
// privacy policy carries, applied to the messages the command actually sends.
// A lowered period must not read as reaching into the archives.
func TestRetentionMessagesNeverPromiseErasureFromBackups(t *testing.T) {
	forbidden := []string{
		"deleted from the backups", "deleted from backups",
		"removed from the backups", "removed from backups",
		"erased from the backups", "erased from backups",
	}
	for name, req := range retentionRequests() {
		t.Run(name, func(t *testing.T) {
			text := strings.ToLower(req.Text)
			for _, claim := range forbidden {
				if strings.Contains(text, claim) {
					t.Fatalf("the message claims %q, which a setting change cannot deliver", claim)
				}
			}
		})
	}
}

// TestRetentionUsageStatesTheBoundsAndChangesNothing: the refusal is explicit
// (the bounds and both valid shapes), and it carries no value to mistake for
// an applied setting.
func TestRetentionUsageStatesTheBoundsAndChangesNothing(t *testing.T) {
	text := BuildRetentionUsageRequest(700001).Text
	for _, needle := range []string{
		"was not changed",
		"1 and 365",
		"/retention 30",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("the refusal never mentions %q:\n%s", needle, text)
		}
	}
}
