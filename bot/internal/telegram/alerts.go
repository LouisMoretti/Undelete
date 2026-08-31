package telegram

import (
	"fmt"
	"strings"
	"time"
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

// DeletionAlert porte tout ce qui identifie un message supprimé au moment de
// notifier le owner. C'est un contexte d'AFFICHAGE : il ne sert jamais à
// décider si une alerte part ou non (contrainte n°8), uniquement à la rendre
// lisible sans avoir à décoder un chat_id numérique.
//
// ChatTitle/ChatUsername peuvent être vides (chat jamais revu depuis la
// migration 0003, ou chat privé sans @username) : le format prévoit un repli
// pour chacun. FromUserID est un pointeur parce que la colonne from_user_id
// est NULL pour les messages sans expéditeur (canaux, messages de service).
type DeletionAlert struct {
	OwnerTelegramUserID int64
	ChatID              int64
	ChatTitle           string
	ChatUsername        string
	FromDisplay         string
	FromUserID          *int64
	MessageType         string
	// TelegramDate est la date d'envoi Telegram en secondes Unix ; 0 signifie
	// « inconnue » (aucune date réelle ne vaut 0 en pratique).
	TelegramDate int64
	Content      string
}

// BuildDeletionMessageRequests construit les alertes de suppression envoyées
// en production et les découpe selon la limite Telegram en unités UTF-16.
//
// L'enrichissement (en-tête d'identité) a lieu AVANT le découpage : la limite
// de 4096 unités s'applique au texte FINAL, en-tête compris, jamais au seul
// contenu restitué.
func BuildDeletionMessageRequests(alert DeletionAlert) []SendMessageRequest {
	text := buildDeletionText(alert)
	chunks := splitTelegramText(text, telegramTextLimit)
	requests := make([]SendMessageRequest, 0, len(chunks))
	for _, chunk := range chunks {
		requests = append(requests, SendMessageRequest{ChatID: alert.OwnerTelegramUserID, Text: chunk})
	}
	return requests
}

// buildDeletionText compose le texte complet de l'alerte : une ligne de titre,
// l'en-tête d'identité, puis le contenu restitué.
//
// Le chat_id numérique reste TOUJOURS affiché : Telegram ne garantit ni titre
// ni @username sur un chat privé, un libellé seul pourrait donc être vide ou
// ambigu entre deux homonymes.
func buildDeletionText(alert DeletionAlert) string {
	var b strings.Builder
	b.WriteString("Message supprimé récupéré\n")
	b.WriteString("Chat : " + chatLine(alert) + "\n")
	b.WriteString("De : " + senderLine(alert) + "\n")

	messageType := alert.MessageType
	if messageType == "" {
		messageType = "inconnu"
	}
	b.WriteString("Type : " + messageType + "\n")
	b.WriteString("Date : " + formatAlertDate(alert.TelegramDate))

	// Pas de contenu textuel (message_type non textuel des phases suivantes,
	// ou texte vide) : on s'arrête à l'en-tête, le type suffit à décrire ce
	// qui a été supprimé. Le format ne casse donc pas quand les médias
	// arriveront, sans rien préjuger de leur restitution.
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
		// Repli : aucun libellé connu pour ce chat (pas de ligne dans chats,
		// cf. absence de backfill en 0003). L'id seul reste exploitable.
		return fmt.Sprintf("chat %d", alert.ChatID)
	}
	return fmt.Sprintf("%s (%d)", label, alert.ChatID)
}

func senderLine(alert DeletionAlert) string {
	name := alert.FromDisplay
	if name == "" {
		name = "inconnu"
	}
	if alert.FromUserID == nil {
		return name
	}
	return fmt.Sprintf("%s (%d)", name, *alert.FromUserID)
}

// formatAlertDate rend la date d'envoi en UTC, format déterministe
// « 2006-01-02 15:04 UTC » : lisible sans outil, indépendant du fuseau de la
// machine qui construit l'alerte (le worker outbox peut la réémettre bien
// après, et les chunks sont figés en base).
func formatAlertDate(telegramDate int64) string {
	if telegramDate == 0 {
		return "inconnue"
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
