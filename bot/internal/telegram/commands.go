package telegram

import (
	"strings"
	"unicode"
)

// CommandPrivacy is the command that returns the privacy policy.
const CommandPrivacy = "/privacy"

// CommandDeleteMyData is the command that erases the owner's data. Typed alone
// it issues a confirmation code; typed with that code as its argument it spends
// it (cf. internal/erasure).
const CommandDeleteMyData = "/delete_my_data"

// CommandRetention is the command that reads and sets the tenant's retention
// period (users.retention_days, 1 to 365 days). Typed alone it answers the
// current value; typed with a number of days it sets it (cf. internal/users).
const CommandRetention = "/retention"

// ParseCommand returns the normalised command a message starts with, and
// whether the message is a command at all.
//
// Telegram only marks a bot_command entity as such when the slash sits at
// offset 0, which is why a leading space or any preceding word disqualifies
// the message: "tell me about /privacy" is a sentence a contact may write in
// a monitored chat, and it must never trigger anything.
//
// Normalisation covers the two shapes the same command takes on the wire:
// the "@botname" suffix Telegram appends (mandatory in groups, optional
// elsewhere) and the arguments that may follow. Case is folded because
// Telegram clients happily send "/Privacy" when autocorrect capitalises the
// first letter.
func ParseCommand(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}

	command := text
	// Any whitespace ends the command, not a hand-picked list of three: a
	// Telegram client on Windows sends CRLF, and "/privacy\r" would otherwise
	// match no command at all. unicode.IsSpace also covers the non-breaking
	// and ideographic spaces mobile keyboards insert.
	if index := strings.IndexFunc(command, unicode.IsSpace); index >= 0 {
		command = command[:index]
	}
	if index := strings.Index(command, "@"); index >= 0 {
		command = command[:index]
	}

	command = strings.ToLower(command)
	if command == "/" {
		return "", false
	}
	return command, true
}

// CommandArgument returns what follows the command word, trimmed. Empty when
// the message is not a command, or carries nothing after it.
//
// The split point is the same one ParseCommand uses -- the first whitespace
// rune -- so "/delete_my_data ABCD2345" yields the command and its code, while
// "/delete_my_data_extra" is a different command word with no argument at all,
// never this command with a suffix.
//
// The argument is returned verbatim apart from the surrounding whitespace: what
// it means belongs to the command that reads it, and normalising a confirmation
// code here would hide that decision from the package that depends on it.
func CommandArgument(text string) string {
	if _, ok := ParseCommand(text); !ok {
		return ""
	}
	index := strings.IndexFunc(text, unicode.IsSpace)
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(text[index:])
}
