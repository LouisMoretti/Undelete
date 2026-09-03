// Package telegram implements a minimal client for the Telegram Bot API,
// using net/http directly (no community wrapper): the business_* fields
// (Telegram Business / commerce automation) arrive late, or never, in
// third-party libraries, and they are the core of this product.
package telegram

import "encoding/json"

// AllowedUpdates is the EXPLICIT list to pass to getUpdates.
//
// Non-negotiable constraint 1: without an explicit allowed_updates, Telegram
// sends NONE of the business_* events by default (they are not part of the
// update set sent by default to existing bots) -- and this silently, without
// any error. A bot that omitted this parameter would seem to work (API
// connection OK, getUpdates answers 200) while never receiving any message.
var AllowedUpdates = []string{
	"business_connection",
	"business_message",
	"edited_business_message",
	"deleted_business_messages",
}

// apiResponse is the standard envelope of any Bot API response.
type apiResponse[T any] struct {
	OK          bool            `json:"ok"`
	Result      T               `json:"result"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Parameters  *responseParams `json:"parameters,omitempty"`
}

type responseParams struct {
	// RetryAfter is present on 429 errors (rate limit): the number of
	// seconds to wait before retrying. The two loops that call the API —
	// telegram.Poller on getUpdates and outbox.Worker on sendMessage — must
	// respect it strictly rather than applying their own backoff.
	RetryAfter int `json:"retry_after,omitempty"`
}

// Update is an entry returned by getUpdates. Only the business_* fields are
// populated in Phase 1: allowed_updates only asks for those.
type Update struct {
	UpdateID                int64                    `json:"update_id"`
	BusinessConnection      *BusinessConnection      `json:"business_connection,omitempty"`
	BusinessMessage         *Message                 `json:"business_message,omitempty"`
	EditedBusinessMessage   *Message                 `json:"edited_business_message,omitempty"`
	DeletedBusinessMessages *BusinessMessagesDeleted `json:"deleted_business_messages,omitempty"`
}

// BusinessConnection describes a Telegram Business connection established or
// updated by the account holder, from Settings -> Telegram Business ->
// Chatbots.
type BusinessConnection struct {
	ID         string `json:"id"`
	User       User   `json:"user"`
	UserChatID int64  `json:"user_chat_id"`
	Date       int64  `json:"date"`
	IsEnabled  bool   `json:"is_enabled"`
	// Rights is the current Bot API representation. CanReplyLegacy only keeps
	// compatibility with legacy responses where can_reply sat directly on
	// BusinessConnection.
	Rights         *BusinessBotRights `json:"rights,omitempty"`
	CanReplyLegacy bool               `json:"can_reply,omitempty"`
}

// BusinessBotRights represents the rights granted to the bot by the
// connection. Phase 1 only uses can_reply, but the nesting level must match
// the current Bot API so as not to silently persist false.
type BusinessBotRights struct {
	CanReply bool `json:"can_reply,omitempty"`
}

func (c BusinessConnection) CanReply() bool {
	if c.Rights != nil {
		return c.Rights.CanReply
	}
	return c.CanReplyLegacy
}

// User is a minimal Telegram user or bot.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Chat is a minimal Telegram chat (Phase 1: only private chats exposed by
// the Business connection).
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Title     string `json:"title,omitempty"`
}

// Message is a Telegram message, as received via business_message /
// edited_business_message. Phase 1: text only (message_type derived
// downstream, media out of scope -- cf. TODO Phase 2 in
// messages/repository.go).
type Message struct {
	MessageID            int64  `json:"message_id"`
	From                 *User  `json:"from,omitempty"`
	Chat                 Chat   `json:"chat"`
	Date                 int64  `json:"date"`
	Text                 string `json:"text,omitempty"`
	BusinessConnectionID string `json:"business_connection_id,omitempty"`
}

// BusinessMessagesDeleted corresponds to the deleted_business_messages update.
//
// Non-negotiable constraint 6: MessageIDs is an ARRAY. A bulk deletion (the
// user selects several messages then "Delete") arrives in a single Telegram
// update with multiple IDs, never as N separate updates. Never assume
// 1 update = 1 deleted message.
//
// Product note: this update does NOT carry the content of the deleted
// messages, only their identifiers -- hence the need to buffer every message
// as soon as it is received (business_message / edited_business_message) so
// it can be restored here.
type BusinessMessagesDeleted struct {
	BusinessConnectionID string  `json:"business_connection_id"`
	Chat                 Chat    `json:"chat"`
	MessageIDs           []int64 `json:"message_ids"`
}

// getUpdatesRequest serializes the parameters of the long-polling call.
type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

// SendMessageRequest serializes the bot's sendMessage alerts.
//
// Non-negotiable constraint 7: this type must not expose
// business_connection_id. The only sendMessage calls of this client are the
// alerts to the owner — welcome sent by business.Service, deletion
// notification relayed by outbox.Worker from notification_outbox; this
// parameter would send them AS the account holder, INSIDE the monitored
// conversation. Its absence from the type makes that scenario impossible by
// construction.
type SendMessageRequest struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// marshal is a shortcut used by client.go.
func marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
