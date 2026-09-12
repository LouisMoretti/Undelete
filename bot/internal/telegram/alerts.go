package telegram

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

const telegramTextLimit = 4096

const welcomeMessageText = "undelete is connected. All chats reachable through this Telegram Business connection " +
	"will now be backed up automatically, without per-chat selection. " +
	"You will be notified here when a message is deleted."

// BuildWelcomeMessageRequest builds exactly the welcome alert sent in
// production. userChatID takes precedence; userID is the defensive fallback
// for legacy Telegram responses that don't provide user_chat_id.
func BuildWelcomeMessageRequest(userChatID, userID int64) SendMessageRequest {
	if userChatID == 0 {
		userChatID = userID
	}
	return SendMessageRequest{ChatID: userChatID, Text: welcomeMessageText}
}

// DeletionAlert carries everything that identifies a deleted message at the
// time the owner is notified. It is a DISPLAY context: it never decides
// whether an alert is sent or not (constraint 8), it only makes the alert
// readable without having to decode a numeric chat_id.
//
// ChatTitle/ChatUsername can be empty (chat not seen again since migration
// 0003, or private chat without @username): the format provides a fallback
// for each. FromUserID is a pointer because the from_user_id column is NULL
// for messages without a sender (channels, service messages).
type DeletionAlert struct {
	OwnerTelegramUserID int64
	ChatID              int64
	ChatTitle           string
	ChatUsername        string
	FromDisplay         string
	FromUserID          *int64
	MessageType         string
	// TelegramDate is the Telegram send date in Unix seconds; 0 means
	// "unknown" (no real date is ever 0 in practice).
	TelegramDate int64
	Content      string
}

// BuildDeletionMessageRequests builds the deletion alerts sent in production
// and splits them according to the Telegram limit in UTF-16 units.
//
// Enrichment (identity header) happens BEFORE splitting: the 4096-unit limit
// applies to the FINAL text, header included, never to the restored content
// alone.
func BuildDeletionMessageRequests(alert DeletionAlert) []SendMessageRequest {
	text := buildDeletionText(alert)
	chunks := splitTelegramText(text, telegramTextLimit)
	requests := make([]SendMessageRequest, 0, len(chunks))
	for _, chunk := range chunks {
		requests = append(requests, SendMessageRequest{ChatID: alert.OwnerTelegramUserID, Text: chunk})
	}
	return requests
}

// privacyChunkLabelFormat labels every chunk of the /privacy answer with its
// rank and the total, e.g. "Privacy policy (1/2)". It is not decoration: the
// sending loop stops at the first failure, so without the total a truncated
// policy would be indistinguishable from a complete one for the only person
// who receives it.
const privacyChunkLabelFormat = "Privacy policy (%d/%d)"

// privacyLabelSeparator sits between the label and the policy text. Its own
// length counts against the Telegram limit like any other character.
const privacyLabelSeparator = "\n\n"

// BuildPrivacyMessageRequests builds the answer of the /privacy command and
// splits it on the same Telegram limit as any other alert.
//
// ownerTelegramUserID is the ONLY recipient: the policy travels as a direct
// message from the bot to the account holder, never with a
// business_connection_id (the alerts-without-business_connection_id
// constraint), which would post it as the holder inside the monitored
// conversation the command was typed in.
//
// The policy is a long document that will not fit in a single message: the
// split is not a defensive precaution here, it is the normal path, and the
// chunks stay in order. The label is part of the message, so the 4096-unit
// limit applies to the FINAL text, label included -- the same rule the
// identity header of a deletion alert follows.
func BuildPrivacyMessageRequests(ownerTelegramUserID int64, policyText string) []SendMessageRequest {
	// The label announces the total, and the total depends on how much room
	// the label leaves: the two are resolved together. Assuming fewer chunks
	// than there are can only under-reserve, never overflow a budget already
	// used, so the assumption is raised until it holds. The loop terminates
	// because each round raises it strictly and a chunk always carries at
	// least one character.
	total := 1
	chunks := splitPolicyText(policyText, telegramTextLimit-privacyLabelUnits(total))
	for len(chunks) > total {
		total = len(chunks)
		chunks = splitPolicyText(policyText, telegramTextLimit-privacyLabelUnits(total))
	}

	requests := make([]SendMessageRequest, 0, len(chunks))
	for index, chunk := range chunks {
		label := fmt.Sprintf(privacyChunkLabelFormat, index+1, len(chunks))
		requests = append(requests, SendMessageRequest{
			ChatID: ownerTelegramUserID,
			Text:   label + privacyLabelSeparator + chunk,
		})
	}
	return requests
}

// privacyLabelUnits is the room the label of a chunk takes, for an answer made
// of total chunks. The widest label of that answer is the one of its last
// chunk ("(total/total)"): reserving that much keeps every chunk under the
// limit, not just the first nine.
func privacyLabelUnits(total int) int {
	return utf16Units(fmt.Sprintf(privacyChunkLabelFormat, total, total)) + utf16Units(privacyLabelSeparator)
}

// MediaUnavailableNote is appended to the text of a media alert that could not
// carry its files (purged from disk, storage unmounted, file above the Bot API
// limit, definitive Telegram refusal). The owner is told a media existed rather
// than being left with a silent hole -- an alert is never dropped.
const MediaUnavailableNote = "[media unavailable]"

// WithMediaUnavailableNote appends MediaUnavailableNote to text without ever
// exceeding the Bot API text limit (counted in UTF-16 units like every other
// Telegram length). Without this, a text already at 4096 units plus the note
// would be refused with a 400 that the outbox turns into a permanent failure:
// the alert would be lost exactly when it carries the most content.
func WithMediaUnavailableNote(text string) string {
	suffix := "\n\n" + MediaUnavailableNote
	if utf16Units(text)+utf16Units(suffix) <= telegramTextLimit {
		return strings.TrimRight(text, "\n") + suffix
	}
	budget := telegramTextLimit - utf16Units(suffix)
	units := 0
	kept := make([]rune, 0, budget)
	for _, r := range strings.TrimRight(text, "\n") {
		size := utf16.RuneLen(r)
		if size < 1 {
			size = 1
		}
		if units+size > budget {
			break
		}
		kept = append(kept, r)
		units += size
	}
	return string(kept) + suffix
}

// BuildMediaAlertText is the text that travels WITH the media entry of an
// alert: the one-line summary of what is attached. It is written to the outbox
// at deletion time and is what the worker sends, plus MediaUnavailableNote,
// when the files cannot go out.
//
// The identity context (chat, sender, date, caption) is NOT repeated here: it
// was already delivered by the text chunks of the same alert, which the outbox
// sends first (chunk ordering).
func BuildMediaAlertText(mediaTypes []string) string {
	if len(mediaTypes) == 0 {
		return "Attached media"
	}
	suffix := ""
	if len(mediaTypes) > 1 {
		suffix = "s"
	}
	return fmt.Sprintf("Attached media (%d file%s: %s)",
		len(mediaTypes), suffix, strings.Join(mediaTypes, ", "))
}

// buildDeletionText composes the full alert text: a title line, the identity
// header, then the restored content.
//
// The numeric chat_id always stays visible: Telegram guarantees neither a
// title nor a @username on a private chat, so a label alone could be empty or
// ambiguous between two namesakes.
func buildDeletionText(alert DeletionAlert) string {
	var b strings.Builder
	b.WriteString("Recovered deleted message\n")
	b.WriteString("Chat: " + chatLine(alert) + "\n")
	b.WriteString("From: " + senderLine(alert) + "\n")

	messageType := alert.MessageType
	if messageType == "" {
		messageType = "unknown"
	}
	b.WriteString("Type: " + messageType + "\n")
	b.WriteString("Date: " + formatAlertDate(alert.TelegramDate))

	// No textual content (non-text message_type in later phases, or empty
	// text): we stop at the header, the type is enough to describe what was
	// deleted. The format therefore won't break when media arrives, without
	// anticipating how it will be restored.
	if alert.Content != "" {
		b.WriteString("\n\n" + alert.Content)
	}
	return b.String()
}

func chatLine(alert DeletionAlert) string {
	label := alert.ChatTitle
	switch {
	case label != "" && alert.ChatUsername != "":
		label += " (@" + alert.ChatUsername + ")"
	case label == "" && alert.ChatUsername != "":
		label = "@" + alert.ChatUsername
	}
	if label == "" {
		// Fallback: no known label for this chat (no row in chats, cf. the
		// absence of backfill in 0003). The id alone remains usable.
		return fmt.Sprintf("chat %d", alert.ChatID)
	}
	return fmt.Sprintf("%s (%d)", label, alert.ChatID)
}

func senderLine(alert DeletionAlert) string {
	name := alert.FromDisplay
	if name == "" {
		name = "unknown"
	}
	if alert.FromUserID == nil {
		return name
	}
	return fmt.Sprintf("%s (%d)", name, *alert.FromUserID)
}

// formatAlertDate renders the send date in UTC, deterministic format
// "2006-01-02 15:04 UTC": readable without any tool, independent of the
// timezone of the machine building the alert (the outbox worker can resend it
// much later, and the chunks are frozen in the database).
func formatAlertDate(telegramDate int64) string {
	if telegramDate == 0 {
		return "unknown"
	}
	return time.Unix(telegramDate, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// splitPolicyText splits a long document into messages of at most limit UTF-16
// units, cutting on paragraph boundaries rather than wherever the limit falls.
//
// Why: a blind split lands mid-word -- measured on the real policy, it cut
// "purged au|tomatically" in the middle of the sentence saying media archives
// are not purged. A document whose sentences survive the split is the point of
// serving a policy at all.
//
// Paragraphs are filled greedily, and each chunk keeps the blank line that
// followed its last paragraph: the chunks therefore partition the text exactly
// (concatenating them returns the input, byte for byte), and a boundary can
// only ever fall on whitespace. A single paragraph too long for one message has
// no boundary to align on and falls back to the rune-level split, which is
// UTF-16-safe but word-blind.
func splitPolicyText(text string, limit int) []string {
	if text == "" || limit < 1 {
		return nil
	}

	var chunks []string
	var current strings.Builder
	currentUnits := 0
	for _, paragraph := range paragraphSegments(text) {
		units := utf16Units(paragraph)
		if currentUnits+units > limit && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			currentUnits = 0
		}
		if units > limit {
			// current was just flushed (or was empty), so appending the
			// sub-chunks here keeps the document in order.
			chunks = append(chunks, splitTelegramText(paragraph, limit)...)
			continue
		}
		current.WriteString(paragraph)
		currentUnits += units
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

// paragraphSegments cuts text on blank lines, each segment carrying the
// separator that ended it. Keeping the separator with the paragraph it follows
// (rather than dropping it, or re-joining with a canonical "\n\n") is what
// makes the split lossless: the segments concatenate back into the input.
func paragraphSegments(text string) []string {
	var segments []string
	for text != "" {
		index := strings.Index(text, "\n\n")
		if index < 0 {
			segments = append(segments, text)
			break
		}
		// Absorb any further newlines: a separator of three blank lines is one
		// boundary, not an empty paragraph between two of them.
		end := index + len("\n\n")
		for end < len(text) && text[end] == '\n' {
			end++
		}
		segments = append(segments, text[:end])
		text = text[end:]
	}
	return segments
}

// utf16Units counts a string in the unit Telegram enforces its limit in: UTF-16
// code units, where a character outside the BMP counts twice.
func utf16Units(text string) int {
	units := 0
	for _, r := range text {
		units += runeUTF16Units(r)
	}
	return units
}

// runeUTF16Units is the UTF-16 width of a single rune. RuneLen returns -1 for
// what UTF-16 cannot encode (a lone surrogate, an out-of-range value); such a
// rune is counted as one unit, which keeps the bound conservative instead of
// making a chunk look shorter than it is.
func runeUTF16Units(r rune) int {
	units := utf16.RuneLen(r)
	if units < 1 {
		return 1
	}
	return units
}

func splitTelegramText(text string, limit int) []string {
	if text == "" || limit < 1 {
		return nil
	}

	var chunks []string
	current := make([]rune, 0, limit)
	units := 0
	for _, r := range text {
		runeUnits := runeUTF16Units(r)
		if units+runeUnits > limit && len(current) > 0 {
			chunks = append(chunks, string(current))
			current = current[:0]
			units = 0
		}
		current = append(current, r)
		units += runeUnits
	}
	if len(current) > 0 {
		chunks = append(chunks, string(current))
	}
	return chunks
}
