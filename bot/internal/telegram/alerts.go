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

func splitTelegramText(text string, limit int) []string {
	if text == "" || limit < 1 {
		return nil
	}

	var chunks []string
	current := make([]rune, 0, limit)
	units := 0
	for _, r := range text {
		runeUnits := utf16.RuneLen(r)
		if runeUnits < 1 {
			runeUnits = 1
		}
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
