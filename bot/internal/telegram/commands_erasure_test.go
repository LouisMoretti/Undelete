package telegram

import (
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// TestDeleteMyDataIsParsedLikeAnyCommand: the erasure command goes through the
// same reader as /privacy, so the rules that protect one protect the other. The
// entry worth reading twice is "/delete_my_data_extra": an underscore is part of
// a command word, so that is a DIFFERENT command, never this one with a suffix.
func TestDeleteMyDataIsParsedLikeAnyCommand(t *testing.T) {
	tests := []struct {
		text     string
		command  string
		argument string
	}{
		{text: "/delete_my_data", command: CommandDeleteMyData},
		{text: "/Delete_My_Data", command: CommandDeleteMyData},
		{text: "/delete_my_data@undelete_bot", command: CommandDeleteMyData},
		{text: "/delete_my_data ABCD2345", command: CommandDeleteMyData, argument: "ABCD2345"},
		{text: "/delete_my_data@undelete_bot ABCD2345", command: CommandDeleteMyData, argument: "ABCD2345"},
		{text: "/delete_my_data\r\nABCD2345", command: CommandDeleteMyData, argument: "ABCD2345"},
		{text: "/delete_my_data   ABCD2345  ", command: CommandDeleteMyData, argument: "ABCD2345"},
		// Different command words: they must not reach the erasure at all.
		{text: "/delete_my_data_extra", command: "/delete_my_data_extra"},
		{text: "/delete_my_data_extra ABCD2345", command: "/delete_my_data_extra", argument: "ABCD2345"},
		{text: "/delete_my_dat", command: "/delete_my_dat"},
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

// TestCommandArgumentIgnoresNonCommands: a sentence that merely quotes the
// command must yield nothing, exactly as ParseCommand refuses it. Otherwise a
// contact writing "tell me about /delete_my_data ABCD2345" would be handing an
// argument to a command that was never issued.
func TestCommandArgumentIgnoresNonCommands(t *testing.T) {
	for _, text := range []string{"", "hello", "tell me about /delete_my_data ABCD2345", " /delete_my_data ABCD2345", "/"} {
		if argument := CommandArgument(text); argument != "" {
			t.Fatalf("CommandArgument(%q) = %q, want empty", text, argument)
		}
	}
}

// erasureRequests is every message the erasure command can send, built the way
// production builds them.
func erasureRequests() map[string]SendMessageRequest {
	const owner = int64(700001)
	return map[string]SendMessageRequest{
		"challenge":    BuildErasureChallengeRequest(owner, "ABCD2345", 10*time.Minute),
		"confirmation": BuildErasureConfirmationRequest(owner, 14),
		"replay":       BuildErasureReplayRequest(owner, 14),
		"expired":      BuildErasureNoticeRequest(owner, ErasureExpiredNotice),
		"unknown":      BuildErasureNoticeRequest(owner, ErasureUnknownNotice),
		"failed":       BuildErasureNoticeRequest(owner, ErasureFailedNotice),
	}
}

// TestErasureMessagesGoToTheOwnerAndFitTheLimit covers the wire contract shared
// by all of them: one message, addressed to the owner's own Telegram id, never
// empty, and within the Bot API limit -- a confirmation split in two could
// deliver the promise without the backup caveat that qualifies it.
func TestErasureMessagesGoToTheOwnerAndFitTheLimit(t *testing.T) {
	for name, req := range erasureRequests() {
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

// TestErasureMessagesNeverPromiseErasureFromBackups is the same guard the
// privacy policy carries, applied to the messages the command actually sends.
// The policy is reviewed; these strings are read once, by the one person whose
// data just went, at the moment they most need the statement to be true.
func TestErasureMessagesNeverPromiseErasureFromBackups(t *testing.T) {
	forbidden := []string{
		"deleted from the backups", "deleted from backups",
		"removed from the backups", "removed from backups",
		"erased from the backups", "erased from backups",
	}
	for name, req := range erasureRequests() {
		t.Run(name, func(t *testing.T) {
			text := strings.ToLower(req.Text)
			for _, claim := range forbidden {
				if strings.Contains(text, claim) {
					t.Fatalf("the message claims %q, which a deletion cannot deliver", claim)
				}
			}
		})
	}
}

// TestTheConfirmationStatesTheResidualSurvival: the acceptance criterion of the
// issue. The confirmation must name the configured retention, in days, and say
// the media archives are not purged automatically -- the one number a "your
// data is gone" message cannot honestly leave out.
//
// F7: the number alone is not the criterion. scripts/backup.sh only deletes
// what its daily run reaches (a missed day moves every deletion by a day),
// so the message must state the number as a CONDITIONAL cleanup target --
// "only while that job runs" -- and never as an unconditional maximum. This
// test asserts that qualification, i.e. the operational truth, not the mere
// presence of the number.
func TestTheConfirmationStatesTheResidualSurvival(t *testing.T) {
	for _, days := range []int{7, 14, 90} {
		for name, text := range map[string]string{
			"confirmation": BuildErasureConfirmationRequest(700001, days).Text,
			"replay":       BuildErasureReplayRequest(700001, days).Text,
		} {
			t.Run(name, func(t *testing.T) {
				for _, needle := range []string{
					"BACKUP_RETENTION_DAYS",
					"not purged automatically",
					"older than " + itoa(days) + " days",
					"only while that job runs",
					"misses moves every deletion by a day",
				} {
					if !strings.Contains(text, needle) {
						t.Fatalf("with %d days, the message never mentions %q", days, needle)
					}
				}
				for _, forbidden := range []string{
					"maximum residual survival",
				} {
					if strings.Contains(text, forbidden) {
						t.Fatalf("with %d days, the message still claims %q, which the daily purge cannot guarantee", days, forbidden)
					}
				}
			})
		}
	}
}

// TestTheChallengeSaysHowToConfirmAndForHowLong: the message is the only
// instructions the owner gets, and a code without the command that spends it,
// or without its validity, is a dead end.
func TestTheChallengeSaysHowToConfirmAndForHowLong(t *testing.T) {
	text := BuildErasureChallengeRequest(700001, "ABCD2345", 10*time.Minute).Text
	for _, needle := range []string{"/delete_my_data ABCD2345", "10 minutes", "once"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("the challenge never mentions %q:\n%s", needle, text)
		}
	}
}

// itoa keeps the assertions above free of a strconv import for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
