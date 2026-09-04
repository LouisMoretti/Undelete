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
// edited_business_message. Text and every media type documented by the Bot
// API for a Business message are decoded here; the consolidated form used
// downstream is produced by ExtractMedia (cf. media.go).
//
// Media fields are mutually exclusive in practice — Telegram sends one
// attachment per message, an album being N messages sharing the same
// media_group_id — but nothing in the API guarantees it, so ExtractMedia
// reads them all rather than stopping at the first match.
type Message struct {
	MessageID            int64  `json:"message_id"`
	From                 *User  `json:"from,omitempty"`
	Chat                 Chat   `json:"chat"`
	Date                 int64  `json:"date"`
	Text                 string `json:"text,omitempty"`
	BusinessConnectionID string `json:"business_connection_id,omitempty"`

	// MediaGroupID ties together the messages of one album. Empty on a
	// standalone media.
	MediaGroupID string `json:"media_group_id,omitempty"`
	// Caption is the text attached to a media (Text stays empty in that
	// case: Telegram never populates both).
	Caption         string          `json:"caption,omitempty"`
	CaptionEntities []MessageEntity `json:"caption_entities,omitempty"`

	// Photo is the ONLY media sent as an array: the same image in several
	// resolutions. Cf. selectLargestPhoto for the retained size.
	Photo     []PhotoSize `json:"photo,omitempty"`
	Video     *Video      `json:"video,omitempty"`
	Animation *Animation  `json:"animation,omitempty"`
	Document  *Document   `json:"document,omitempty"`
	Audio     *Audio      `json:"audio,omitempty"`
	Voice     *Voice      `json:"voice,omitempty"`
	VideoNote *VideoNote  `json:"video_note,omitempty"`
	Sticker   *Sticker    `json:"sticker,omitempty"`

	// raw keeps the bytes of the message object as received, so ExtractMedia
	// can surface a media type this struct does not know yet instead of
	// silently dropping it (cf. MediaTypeUnknown). It is unexported: it takes
	// no part in serialization and stays invisible to the callers.
	raw json.RawMessage
}

// UnmarshalJSON decodes a Message normally, then keeps a copy of the raw
// object for the unknown-media fallback.
func (m *Message) UnmarshalJSON(data []byte) error {
	// messageAlias drops the methods of Message, UnmarshalJSON included:
	// without it, encoding/json would call this function recursively.
	type messageAlias Message
	var alias messageAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*m = Message(alias)
	m.raw = append(json.RawMessage(nil), data...)
	return nil
}

// MessageEntity is a special entity of a text or a caption (mention, URL,
// bold, custom emoji...). Kept whole so a restored caption can be re-rendered
// with its formatting.
type MessageEntity struct {
	Type          string `json:"type"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	URL           string `json:"url,omitempty"`
	User          *User  `json:"user,omitempty"`
	Language      string `json:"language,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

// PhotoSize is one resolution of a photo, or the thumbnail of another media.
// FileSize is optional: Telegram omits it on some sizes, hence the absence of
// any "size 0 = empty file" meaning.
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Video is a video file.
type Video struct {
	FileID            string     `json:"file_id"`
	FileUniqueID      string     `json:"file_unique_id"`
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	Duration          int        `json:"duration"`
	Thumbnail         *PhotoSize `json:"thumbnail,omitempty"`
	FileName          string     `json:"file_name,omitempty"`
	MimeType          string     `json:"mime_type,omitempty"`
	FileSize          int64      `json:"file_size,omitempty"`
	SupportsStreaming bool       `json:"supports_streaming,omitempty"`
}

// Animation is a GIF, or an H.264/MPEG-4 AVC video without sound. Telegram
// sends it as its own type, never as a Video.
type Animation struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	Duration     int        `json:"duration"`
	Thumbnail    *PhotoSize `json:"thumbnail,omitempty"`
	FileName     string     `json:"file_name,omitempty"`
	MimeType     string     `json:"mime_type,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
}

// Document is any file not matching a more specific type.
type Document struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	Thumbnail    *PhotoSize `json:"thumbnail,omitempty"`
	FileName     string     `json:"file_name,omitempty"`
	MimeType     string     `json:"mime_type,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
}

// Audio is a music file, as opposed to Voice.
type Audio struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	Duration     int        `json:"duration"`
	Performer    string     `json:"performer,omitempty"`
	Title        string     `json:"title,omitempty"`
	FileName     string     `json:"file_name,omitempty"`
	MimeType     string     `json:"mime_type,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
	Thumbnail    *PhotoSize `json:"thumbnail,omitempty"`
}

// Voice is a voice message. It carries neither dimensions nor thumbnail.
type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// VideoNote is a round video message. Length is the side of the square video,
// which is why it feeds both Width and Height downstream.
type VideoNote struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	Length       int        `json:"length"`
	Duration     int        `json:"duration"`
	Thumbnail    *PhotoSize `json:"thumbnail,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
}

// Sticker is a sticker. Type is the sticker's own kind ("regular", "mask",
// "custom_emoji") and must not be confused with MediaAttachment.Type.
type Sticker struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	Type         string     `json:"type,omitempty"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	IsAnimated   bool       `json:"is_animated,omitempty"`
	IsVideo      bool       `json:"is_video,omitempty"`
	Thumbnail    *PhotoSize `json:"thumbnail,omitempty"`
	Emoji        string     `json:"emoji,omitempty"`
	SetName      string     `json:"set_name,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
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
