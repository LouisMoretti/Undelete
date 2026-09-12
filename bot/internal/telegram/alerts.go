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

// The /delete_my_data texts. They live here, next to the welcome message and
// the privacy chunk label, for the reason the whole package exists: every
// string the bot sends is built in one place, where the wire contract (a direct
// message to the owner, never a business_connection_id -- impossible by
// construction on SendMessageRequest) is testable without a database.
//
// Each of them is a single message: unlike the policy, none comes close to the
// 4096-unit limit, and a confirmation split in two would let the owner receive
// the promise without the caveat that qualifies it.
const (
	// erasureBackupCaveat is the sentence the issue is really about, and it is
	// deliberately phrased as a survival TIME rather than as a promise. A
	// deletion in the database cannot rewrite an archive already written, so the
	// only honest statement is how long those archives live -- and the media
	// archives, which nothing purges automatically, are named rather than
	// rounded into the same number.
	erasureBackupCaveat = "Backups are the exception, and the limit is worth stating plainly: a deletion " +
		"here cannot rewrite an archive that was already written. Database dumps are purged after " +
		"%d days (BACKUP_RETENTION_DAYS), which is therefore the maximum residual survival of what " +
		"was just deleted. Media archives are not purged automatically: they survive until the " +
		"operator deletes them."

	erasureChallengeText = "Data erasure requested.\n\n" +
		"To confirm, send this exact command within %d minutes:\n\n" +
		"%s %s\n\n" +
		"It deletes, for this account: every saved message, every chat label, every stored " +
		"attachment on disk and every alert still queued. The Business connections are disabled " +
		"first, so nothing new is captured while it runs.\n\n" +
		"The code works once and only for you. Doing nothing cancels it: the code simply expires."

	erasureConfirmationText = "Data erasure complete.\n\n" +
		"Your Business connections are disabled, and every saved message, chat label, stored " +
		"attachment and queued alert of this account is gone from the database and from the disk " +
		"of this instance.\n\n" +
		"%s\n\n" +
		"Reconnecting undelete from your Telegram settings starts a new capture from zero; nothing " +
		"that was deleted comes back."

	erasureReplayText = "Nothing left to erase.\n\n" +
		"This confirmation code was already used, and the data it covered is already gone. Nothing " +
		"was deleted a second time.\n\n" +
		"%s"

	// ErasureExpiredNotice and ErasureUnknownNotice both send the owner back to
	// the same place, and neither says which of the two happened in a way a
	// third party could use: the answer only ever reaches the owner anyway.
	ErasureExpiredNotice = "This confirmation code has expired. A code is valid for a few minutes only; " +
		"send /delete_my_data again to get a new one. Nothing was deleted."

	ErasureUnknownNotice = "This confirmation code is not valid. Send /delete_my_data to get a new one. " +
		"Nothing was deleted."

	// ErasureFailedNotice is the honest answer to a half-done erasure: the
	// deletion is resumable, the same code still spends it, and what is already
	// deleted is not coming back.
	ErasureFailedNotice = "The erasure did not finish. Part of your data may already be deleted, and what " +
		"is deleted does not come back. Send the same /delete_my_data command with the same code " +
		"again to resume it."
)

// BuildErasureChallengeRequest builds the message that hands the owner a
// confirmation code.
//
// The code travels to the owner's private conversation with the bot, but the
// confirmation is typed back in a monitored chat -- the Bot API delivers no
// other kind of message -- where the contact sees it. That exposure is bounded
// by the short validity window and made harmless by the sender check: a contact
// re-typing the code is not the owner of the connection, so the bot never even
// reads their command as one.
func BuildErasureChallengeRequest(ownerTelegramUserID int64, code string, ttl time.Duration) SendMessageRequest {
	minutes := int(ttl.Round(time.Minute) / time.Minute)
	return SendMessageRequest{
		ChatID: ownerTelegramUserID,
		Text:   fmt.Sprintf(erasureChallengeText, minutes, CommandDeleteMyData, code),
	}
}

// BuildErasureConfirmationRequest builds the final confirmation, which states
// the maximum residual survival in the backups without ever promising an
// erasure OF them.
func BuildErasureConfirmationRequest(ownerTelegramUserID int64, backupRetentionDays int) SendMessageRequest {
	return SendMessageRequest{
		ChatID: ownerTelegramUserID,
		Text:   fmt.Sprintf(erasureConfirmationText, fmt.Sprintf(erasureBackupCaveat, backupRetentionDays)),
	}
}

// BuildErasureReplayRequest answers a confirmation code submitted again after
// its erasure already completed. It repeats the backup caveat rather than
// referring back to a message the owner may no longer have in view.
func BuildErasureReplayRequest(ownerTelegramUserID int64, backupRetentionDays int) SendMessageRequest {
	return SendMessageRequest{
		ChatID: ownerTelegramUserID,
		Text:   fmt.Sprintf(erasureReplayText, fmt.Sprintf(erasureBackupCaveat, backupRetentionDays)),
	}
}

// BuildErasureNoticeRequest wraps one of the Erasure*Notice texts into the same
// owner-only, connection-less envelope as every other answer.
func BuildErasureNoticeRequest(ownerTelegramUserID int64, notice string) SendMessageRequest {
	return SendMessageRequest{ChatID: ownerTelegramUserID, Text: notice}
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
