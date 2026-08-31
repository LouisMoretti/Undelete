// Package telegram implémente un client minimal pour la Bot API Telegram,
// en net/http direct (pas de wrapper communautaire) : les champs business_*
// (Telegram Business / Automatisation d'échange) arrivent en retard, voire
// jamais, dans les libs tierces, et ils sont le cœur de ce produit.
package telegram

import "encoding/json"

// AllowedUpdates est la liste EXPLICITE à passer à getUpdates.
//
// Contrainte non négociable n°1 : sans allowed_updates explicite, Telegram
// n'envoie AUCUN des événements business_* par défaut (ils ne font pas
// partie du set d'updates envoyé par défaut aux bots existants) -- et ce
// silencieusement, sans erreur. Un bot qui omettrait ce paramètre semblerait
// fonctionner (connexion à l'API OK, getUpdates répond 200) tout en ne
// recevant jamais aucun message.
var AllowedUpdates = []string{
	"business_connection",
	"business_message",
	"edited_business_message",
	"deleted_business_messages",
}

// apiResponse est l'enveloppe standard de toute réponse de la Bot API.
type apiResponse[T any] struct {
	OK          bool            `json:"ok"`
	Result      T               `json:"result"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Parameters  *responseParams `json:"parameters,omitempty"`
}

type responseParams struct {
	// RetryAfter est présent sur les erreurs 429 (rate limit) : nombre de
	// secondes à attendre avant de réessayer. Les deux boucles qui appellent
	// l'API — telegram.Poller sur getUpdates et outbox.Worker sur sendMessage
	// — doivent le respecter strictement plutôt que d'appliquer leur propre
	// backoff.
	RetryAfter int `json:"retry_after,omitempty"`
}

// Update est une entrée renvoyée par getUpdates. Seuls les champs business_*
// sont peuplés côté Phase 1 : allowed_updates ne demande que ceux-là.
type Update struct {
	UpdateID                int64                    `json:"update_id"`
	BusinessConnection      *BusinessConnection      `json:"business_connection,omitempty"`
	BusinessMessage         *Message                 `json:"business_message,omitempty"`
	EditedBusinessMessage   *Message                 `json:"edited_business_message,omitempty"`
	DeletedBusinessMessages *BusinessMessagesDeleted `json:"deleted_business_messages,omitempty"`
}

// BusinessConnection décrit une connexion Telegram Business établie ou mise
// à jour par le titulaire du compte, depuis Réglages -> Telegram Business ->
// Chatbots.
type BusinessConnection struct {
	ID         string `json:"id"`
	User       User   `json:"user"`
	UserChatID int64  `json:"user_chat_id"`
	Date       int64  `json:"date"`
	IsEnabled  bool   `json:"is_enabled"`
	// Rights est la représentation Bot API actuelle. CanReplyLegacy garde
	// seulement la compatibilité avec d'anciennes réponses où can_reply
	// figurait directement sur BusinessConnection.
	Rights         *BusinessBotRights `json:"rights,omitempty"`
	CanReplyLegacy bool               `json:"can_reply,omitempty"`
}

// BusinessBotRights représente les droits accordés au bot par la connexion.
// Phase 1 n'utilise que can_reply, mais le niveau d'imbrication doit être
// fidèle à la Bot API actuelle pour ne pas persister false silencieusement.
type BusinessBotRights struct {
	CanReply bool `json:"can_reply,omitempty"`
}

func (c BusinessConnection) CanReply() bool {
	if c.Rights != nil {
		return c.Rights.CanReply
	}
	return c.CanReplyLegacy
}

// User est un utilisateur ou bot Telegram minimal.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Chat est un chat Telegram minimal (Phase 1 : uniquement des chats privés
// exposés par la connexion Business).
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Title     string `json:"title,omitempty"`
}

// Message est un message Telegram, tel que reçu via business_message /
// edited_business_message. Phase 1 : texte uniquement (message_type dérivé
// en aval, médias hors périmètre -- cf. TODO Phase 2 dans messages/repository.go).
type Message struct {
	MessageID            int64  `json:"message_id"`
	From                 *User  `json:"from,omitempty"`
	Chat                 Chat   `json:"chat"`
	Date                 int64  `json:"date"`
	Text                 string `json:"text,omitempty"`
	BusinessConnectionID string `json:"business_connection_id,omitempty"`
}

// BusinessMessagesDeleted correspond à l'update deleted_business_messages.
//
// Contrainte non négociable n°6 : MessageIDs est un TABLEAU. Une suppression
// groupée (l'utilisateur sélectionne plusieurs messages puis "Supprimer")
// arrive en un seul update Telegram avec plusieurs IDs, jamais en N
// updates séparés. Ne jamais supposer 1 update = 1 message supprimé.
//
// Note produit : cet update NE transporte PAS le contenu des messages
// supprimés, seulement leurs identifiants -- d'où la nécessité de
// bufferiser chaque message dès sa réception (business_message /
// edited_business_message) pour pouvoir le restituer ici.
type BusinessMessagesDeleted struct {
	BusinessConnectionID string  `json:"business_connection_id"`
	Chat                 Chat    `json:"chat"`
	MessageIDs           []int64 `json:"message_ids"`
}

// getUpdatesRequest sérialise les paramètres de l'appel long-polling.
type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

// SendMessageRequest sérialise les alertes sendMessage du bot.
//
// Contrainte non négociable n°7 : ce type ne doit pas exposer
// business_connection_id. Les seuls sendMessage de ce client sont les alertes
// au owner — bienvenue émise par business.Service, notification de suppression
// relayée par outbox.Worker depuis notification_outbox ; ce paramètre les
// enverrait EN TANT QUE le titulaire, DANS la conversation surveillée. Son
// absence du type rend ce scénario impossible par construction.
type SendMessageRequest struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// marshal est un raccourci utilisé par client.go.
func marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
