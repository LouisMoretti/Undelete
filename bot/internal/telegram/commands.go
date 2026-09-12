package telegram

import (
	"strings"
	"unicode"
)

// CommandPrivacy is the command that returns the privacy policy.
const CommandPrivacy = "/privacy"

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
