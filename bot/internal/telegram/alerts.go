package telegram

import (
	"fmt"
	"unicode/utf16"
)

const telegramTextLimit = 4096

const welcomeMessageText = "undelete est connecté. Tous les chats accessibles via cette connexion Telegram Business " +
	"seront désormais sauvegardés automatiquement, sans sélection possible par conversation. " +
	"Vous serez notifié ici en cas de suppression."

// BuildWelcomeMessageRequest construit exactement l'alerte de bienvenue envoyée
// en production. userChatID est prioritaire ; userID est le repli défensif pour
// les anciennes réponses Telegram qui ne fournissent pas user_chat_id.
func BuildWelcomeMessageRequest(userChatID, userID int64) SendMessageRequest {
	if userChatID == 0 {
		userChatID = userID
	}
	return SendMessageRequest{ChatID: userChatID, Text: welcomeMessageText}
}

// BuildDeletionMessageRequests construit les alertes de suppression envoyées
// en production et les découpe selon la limite Telegram en unités UTF-16.
func BuildDeletionMessageRequests(ownerTelegramUserID, chatID int64, content string) []SendMessageRequest {
	text := fmt.Sprintf("Message supprimé récupéré (chat %d) :\n\n%s", chatID, content)
	chunks := splitTelegramText(text, telegramTextLimit)
	requests := make([]SendMessageRequest, 0, len(chunks))
	for _, chunk := range chunks {
		requests = append(requests, SendMessageRequest{ChatID: ownerTelegramUserID, Text: chunk})
	}
	return requests
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
