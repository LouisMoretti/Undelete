// Package app wires up dependencies and routes each Telegram Update to the
// appropriate business handling. It is the only place in the code that knows
// the full incoming flow (connection resolution -> save -> outbox enqueue).
// The outgoing flow belongs to outbox.Worker.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/erasure"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/privacy"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// businessService is the subset of business.Service used by Handler: resolving
// a connection for save/delete, and processing connection updates. An
// interface (rather than the concrete type) so unit tests can substitute a
// fake without a database.
type businessService interface {
	Resolve(ctx context.Context, connectionID string) (*business.Connection, error)
	HandleBusinessConnection(ctx context.Context, tc telegram.BusinessConnection) error
}

// messageStore is the subset of messages.Repository used by Handler.
type messageStore interface {
	Save(ctx context.Context, ownerUserID int64, m messages.Record, edited bool) error
	MarkDeleted(ctx context.Context, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID int64, messageIDs []int64) ([]messages.DeletedRecord, error)
}

// mediaCatalogue is the subset of media.Repository used by Handler: the
// pending-row catalogue written at save time (the download itself belongs to
// the fetch loop). Nil disables the capture entirely (text-only mode).
type mediaCatalogue interface {
	Save(ctx context.Context, ownerUserID int64, m media.Record) (int64, error)
}

// alertSender is the subset of telegram.Client used to answer a command. The
// same method the welcome message goes through: a direct send to the account
// holder, never durable delivery through the outbox -- a command answer that
// is lost is retried by typing the command again, unlike a deletion alert
// whose content no longer exists anywhere else.
type alertSender interface {
	SendMessage(ctx context.Context, req telegram.SendMessageRequest) error
}

// dataEraser is the subset of erasure.Service used by Handler: issue a
// confirmation code, and spend one. An interface rather than the concrete type
// so the command can be exercised without a database or a disk -- every branch
// of it decides whether to destroy a tenant's data.
type dataEraser interface {
	Request(ctx context.Context, t erasure.Tenant) (erasure.Challenge, error)
	Confirm(ctx context.Context, t erasure.Tenant, code string) (erasure.Outcome, error)
}

// Handler routes Telegram Business updates to business handling. Its
// methods are called strictly sequentially by telegram.Poller (constraint
// #5): no mutex protection is needed here, the call order IS the
// consistency guarantee.
type Handler struct {
	business businessService
	messages messageStore
	// media catalogues the attachments of every saved message. Nil disables
	// the capture entirely (text-only mode): the messages keep being saved,
	// and no deletion alert will carry a file.
	media mediaCatalogue
	// sender answers the owner's commands. Nil disables command handling
	// entirely: messages keep being saved, a /privacy simply gets no answer.
	sender alertSender
	// eraser serves /delete_my_data. Nil disables that command alone: the
	// command is then ignored exactly like an unknown one, silently, rather
	// than answered with a promise nothing behind it can keep.
	eraser dataEraser
	// backupRetentionDays is BACKUP_RETENTION_DAYS, the maximum residual
	// survival the erasure confirmation must state. Passed down rather than
	// read from the environment here: the answer has to quote the value this
	// deployment actually purges its dumps with.
	backupRetentionDays int
	logger              *slog.Logger
}

// Option configures a Handler. Used for what is optional by construction (the
// command answer), so the existing call sites keep compiling unchanged.
type Option func(*Handler)

// WithCommandSender enables the answers to the owner's commands (/privacy).
func WithCommandSender(sender alertSender) Option {
	return func(h *Handler) { h.sender = sender }
}

// WithDataEraser enables /delete_my_data. backupRetentionDays is what the
// confirmation states as the maximum residual survival in the backups; it is
// taken alongside the eraser rather than separately so a Handler can never
// answer a completed erasure with a number nobody configured.
func WithDataEraser(eraser dataEraser, backupRetentionDays int) Option {
	return func(h *Handler) {
		h.eraser = eraser
		h.backupRetentionDays = backupRetentionDays
	}
}

func NewHandler(businessSvc businessService, messagesRepo messageStore, mediaRepo mediaCatalogue, logger *slog.Logger, opts ...Option) *Handler {
	h := &Handler{
		business: businessSvc,
		messages: messagesRepo,
		media:    mediaRepo,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HandleUpdate implements telegram.Handler.
func (h *Handler) HandleUpdate(ctx context.Context, u telegram.Update) error {
	switch {
	case u.BusinessConnection != nil:
		return h.business.HandleBusinessConnection(ctx, *u.BusinessConnection)

	case u.BusinessMessage != nil:
		// Control commands are resolved BEFORE the capture filter: a
		// disabled connection still authenticates its owner's
		// /delete_my_data (resume after a crash, replay of a completed
		// erasure), and a confirmation carrying a code is never saved --
		// the code must not land in messages.text_content (and therefore
		// in a dump), whatever the sender or the connection state.
		if handled, err := h.handleControlCommand(ctx, u.BusinessMessage); err != nil {
			return err
		} else if handled {
			return nil
		}
		conn, err := h.saveMessage(ctx, u.BusinessMessage, false)
		if err != nil {
			return err
		}
		if conn == nil {
			// Message ignored upstream (unknown, refused or disabled
			// connection): no context in which a command would be legitimate.
			return nil
		}
		h.answerCommand(ctx, conn, u.BusinessMessage)
		return nil

	case u.EditedBusinessMessage != nil:
		// Deliberately no command handling on an edit: a command is an act,
		// and rewriting an old message into "/privacy" is not one. The one
		// exception is the negative half of the secrecy rule: an edit whose
		// new text IS a confirmation would persist a live code in
		// messages.text_content without ever spending it, so it is dropped
		// before the save, unexecuted.
		if drop, err := h.dropConfirmEdit(ctx, u.EditedBusinessMessage); err != nil {
			return err
		} else if drop {
			return nil
		}
		_, err := h.saveMessage(ctx, u.EditedBusinessMessage, true)
		return err

	case u.DeletedBusinessMessages != nil:
		return h.handleDeleted(ctx, u.DeletedBusinessMessages)

	default:
		// allowed_updates only requests the 4 business_* types: an update
		// of another type should never arrive here. We log and continue
		// rather than failing the processing.
		h.logger.Debug("update ignored: no business_* field populated", slog.Int64("update_id", u.UpdateID))
		return nil
	}
}

// handleControlCommand serves /delete_my_data before the message is saved,
// and reports whether the update was consumed (true: the caller must not save
// it, let alone answer it a second time).
//
// Two properties make the pre-save position necessary, and both are about a
// connection the capture filter would refuse:
//
//   - resumption: step 1 of the erasure disables the tenant's connections, so
//     a code whose erasure was interrupted (or completed) arrives through a
//     disabled connection from then on. Resolving it through saveMessage
//     would drop it as "connection disabled" and strand the tenant with no
//     way to resume or to hear "already erased". The control path therefore
//     resolves the connection itself and serves the owner's command WITHOUT
//     saving the message and WITHOUT re-enabling anything.
//   - secrecy: the confirmation carries the code in clear. Saving the message
//     first would persist that code in messages.text_content -- and every row
//     travels into every pg_dump -- before the erasure (or the sender check)
//     ever runs. A confirmation is therefore never saved, on any connection,
//     by any sender.
//
// Anything that is not the owner's /delete_my_data returns false and flows
// into the normal path: an unknown command or a bare request on an enabled
// connection is a message like any other (saved, then answered), and a
// refused connection stays silent exactly as saveMessage would keep it.
func (h *Handler) handleControlCommand(ctx context.Context, msg *telegram.Message) (bool, error) {
	if msg == nil || msg.BusinessConnectionID == "" || msg.Chat.ID == 0 || msg.MessageID == 0 {
		return false, nil
	}
	conn, err := h.business.Resolve(ctx, msg.BusinessConnectionID)
	if err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			// Refused by the mono-tenant guard: no owner, no command.
			return true, nil
		}
		return false, fmt.Errorf("connection resolution for control command: %w", err)
	}
	command, ok := telegram.ParseCommand(messageText(msg))
	if !ok || command != telegram.CommandDeleteMyData {
		return false, nil
	}
	if msg.From == nil || msg.From.ID != conn.OwnerTelegramUserID {
		// A third party retyping a code they saw in the monitored chat, a
		// message without a sender, or a chat id spoofed to look like the
		// owner's: nothing is sent, nothing reaches the eraser, and -- the
		// point of doing this before the save -- nothing is WRITTEN either.
		// A retried code is a live erasure token; persisting it would store
		// the very secret the challenge table only ever hashes.
		h.logger.Debug("erasure command ignored: sender is not the owner of the connection",
			slog.String("business_connection_id", conn.ID),
			slog.Int64("chat_id", msg.Chat.ID))
		return true, nil
	}

	if code := telegram.CommandArgument(messageText(msg)); code != "" {
		h.handleDeleteMyData(ctx, conn, messageText(msg))
		return true, nil
	}
	if conn.IsEnabled {
		// A bare request on a live connection stays a message like any
		// other: it carries no secret, so the normal path saves it and
		// answers it.
		return false, nil
	}
	// A bare request on a disabled connection (the state an erasure leaves
	// behind): issue a fresh code without saving anything.
	h.handleDeleteMyData(ctx, conn, messageText(msg))
	return true, nil
}

// dropConfirmEdit reports whether an edited message must not be saved: its
// new text is a /delete_my_data confirmation, i.e. a live erasure token in
// clear. Edits never execute commands, so dropping it loses no act -- only
// the secret the save would otherwise persist. Anything else (including a
// bare /delete_my_data, which carries no code) saves normally.
func (h *Handler) dropConfirmEdit(ctx context.Context, msg *telegram.Message) (bool, error) {
	if msg == nil || msg.BusinessConnectionID == "" || msg.Chat.ID == 0 || msg.MessageID == 0 {
		return false, nil
	}
	text := messageText(msg)
	command, ok := telegram.ParseCommand(text)
	if !ok || command != telegram.CommandDeleteMyData || telegram.CommandArgument(text) == "" {
		return false, nil
	}
	if _, err := h.business.Resolve(ctx, msg.BusinessConnectionID); err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			return true, nil
		}
		return false, fmt.Errorf("connection resolution for edited command: %w", err)
	}
	h.logger.Debug("edited confirmation dropped before the save: a code is never stored",
		slog.String("business_connection_id", msg.BusinessConnectionID))
	return true, nil
}

// saveMessage always saves the received message. It returns the connection
// the message belongs to, or nil when the message was ignored (unknown,
// refused or disabled connection) -- the caller needs that distinction to
// decide whether a command carried by this message deserves an answer.
//
// Constraint #8: NO chat_id condition here, and no consultation of any
// preference table -- an active Business connection automatically covers
// all chats Telegram exposes to it. The only filter applied is
// business.Service.Resolve (does the connection exist and is_enabled),
// never a per-conversation filter.
func (h *Handler) saveMessage(ctx context.Context, msg *telegram.Message, edited bool) (*business.Connection, error) {
	if msg == nil {
		return nil, nil
	}
	if msg.BusinessConnectionID == "" {
		h.logger.Debug("message ignored: empty business_connection_id")
		return nil, nil
	}
	if msg.Chat.ID == 0 || msg.MessageID == 0 {
		h.logger.Debug("message ignored: zero chat_id or message_id",
			slog.Int64("chat_id", msg.Chat.ID),
			slog.Int64("message_id", msg.MessageID))
		return nil, nil
	}
	conn, err := h.business.Resolve(ctx, msg.BusinessConnectionID)
	if err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			h.logger.Debug("message ignored: connection refused by the mono-tenant guard",
				slog.String("business_connection_id", msg.BusinessConnectionID))
			return nil, nil
		}
		return nil, fmt.Errorf("connection resolution for save: %w", err)
	}
	if !conn.IsEnabled {
		h.logger.Debug("message ignored: connection disabled",
			slog.String("business_connection_id", conn.ID))
		return nil, nil
	}

	var fromUserID *int64
	fromDisplay := ""
	if msg.From != nil {
		id := msg.From.ID
		fromUserID = &id
		fromDisplay = displayName(msg.From)
	}

	attachments := telegram.ExtractMedia(msg)

	record := messages.Record{
		BusinessConnectionID: msg.BusinessConnectionID,
		ChatID:               msg.Chat.ID,
		MessageID:            msg.MessageID,
		FromUserID:           fromUserID,
		FromDisplay:          fromDisplay,
		MessageType:          messageType(attachments),
		// A media message carries its text in caption, never in text: storing
		// it in the same column keeps ONE place holding what was written, which
		// the alert then restores both in its text and as the caption of the
		// file itself.
		TextContent:  messageText(msg),
		TelegramDate: msg.Date,
		ChatTitle:    chatTitle(msg.Chat),
		ChatUsername: msg.Chat.Username,
		ChatType:     msg.Chat.Type,
	}

	if err := h.messages.Save(ctx, conn.OwnerUserID, record, edited); err != nil {
		return nil, fmt.Errorf("message save: %w", err)
	}

	if err := h.saveMedia(ctx, conn.OwnerUserID, msg, attachments); err != nil {
		return nil, err
	}

	// Logs: ids, types, counters only. NEVER msg.Text nor any user
	// content -- a product constraint, not a style preference: logging the
	// content would replicate every monitored conversation into the
	// application logs.
	h.logger.Info("message saved",
		slog.String("business_connection_id", conn.ID),
		slog.Int64("chat_id", msg.Chat.ID),
		slog.Int64("message_id", msg.MessageID),
		slog.Int("attachments", len(attachments)),
		slog.Bool("edited", edited))

	return conn, nil
}

// answerCommand answers the commands the account holder types in a chat
// covered by the connection.
//
// Why a monitored chat and not a direct conversation with the bot: the
// allowed_updates list is the four business_* types (the explicit
// allowed_updates constraint), so a plain `message` addressed to the bot is
// never delivered. What the bot does receive is every business_message, the
// holder's own outgoing messages included -- that is where the command is read
// from.
//
// The answer goes out as a direct message from the bot to the holder, on
// their own Telegram id, and NEVER carries a business_connection_id (the
// alerts-without-business_connection_id constraint, enforced by
// SendMessageRequest itself): the policy must not appear, signed by the holder,
// in the conversation where it was typed.
//
// Only the holder is answered. A contact who writes /privacy in a monitored
// chat gets nothing at all: not an answer in the chat, not an answer to
// themselves, not a notification. Two independent checks stand in the way --
// the connection resolution, which the mono-tenant guard already filters, and
// the sender identity compared against the owner of that very connection.
func (h *Handler) answerCommand(ctx context.Context, conn *business.Connection, msg *telegram.Message) {
	if h.sender == nil {
		return
	}

	command, ok := telegram.ParseCommand(messageText(msg))
	if !ok {
		return
	}
	if msg.From == nil || msg.From.ID != conn.OwnerTelegramUserID {
		// A third party, or a message without a sender. Logged with ids only,
		// never the command text.
		h.logger.Debug("command ignored: sender is not the owner of the connection",
			slog.String("business_connection_id", conn.ID),
			slog.Int64("chat_id", msg.Chat.ID))
		return
	}

	switch command {
	case telegram.CommandPrivacy:
		h.sendPrivacyPolicy(ctx, conn)
	case telegram.CommandDeleteMyData:
		h.handleDeleteMyData(ctx, conn, messageText(msg))
	default:
		h.logger.Debug("unknown command ignored",
			slog.String("business_connection_id", conn.ID))
	}
}

// commandAnswerTimeout bounds the whole answer to one command, every chunk and
// every Telegram retry included.
//
// Why a bound at all: this runs on the poller's single goroutine (the
// sequential-update-processing invariant), so getUpdates is blocked until it
// returns, and telegram.Client retries three times while honouring an
// unbounded 429 retry_after. Without a ceiling, one /privacy could park the
// poller on Telegram's backoff and delay the deleted_business_messages updates
// that carry content existing nowhere else.
// Ten seconds is generous for two sendMessage calls and still short enough
// that a deletion arriving meanwhile is handled within the same poll cycle.
const commandAnswerTimeout = 10 * time.Second

// sendPrivacyPolicy delivers the policy, split into chunks that respect the
// Telegram limit in UTF-16 units and each labelled "Privacy policy (i/n)".
//
// A failed send is logged and stops the remaining chunks: sending the rest
// would leave the holder with a document missing its middle, and a command is
// retried by typing it again. The label is what makes that visible on the
// receiving side -- a policy stopping at "(1/2)" reads as incomplete, where an
// unlabelled one would look whole. Nothing is returned to the poller either --
// an undelivered policy must never make an update look like it failed, still
// less replay the message save that preceded it. A timeout is just one more
// send failure: same log, same silence towards the poller.
func (h *Handler) sendPrivacyPolicy(ctx context.Context, conn *business.Connection) {
	requests := telegram.BuildPrivacyMessageRequests(conn.OwnerTelegramUserID, privacy.Text())
	if !h.sendCommandAnswer(ctx, conn, "privacy policy", requests) {
		return
	}

	h.logger.Info("privacy policy sent",
		slog.String("business_connection_id", conn.ID),
		slog.String("policy_version", privacy.Version()),
		slog.String("policy_effective_date", privacy.EffectiveDate()),
		slog.Int("chunks", len(requests)))
}

// sendCommandAnswer delivers the chunks of one command answer under
// commandAnswerTimeout and reports whether all of them went out.
//
// Shared by every command rather than duplicated per command: the deadline is
// the thing that keeps the poller moving, and a second copy of it is a second
// place for it to be forgotten. The stop-at-first-failure rule is shared for
// the same reason as it exists for /privacy -- half an answer is worse than
// none, and a command is retried by typing it again.
func (h *Handler) sendCommandAnswer(ctx context.Context, conn *business.Connection, what string, requests []telegram.SendMessageRequest) bool {
	ctx, cancel := context.WithTimeout(ctx, commandAnswerTimeout)
	defer cancel()

	for index, req := range requests {
		if err := h.sender.SendMessage(ctx, req); err != nil {
			h.logger.Error("failed to send a command answer",
				slog.String("answer", what),
				slog.String("business_connection_id", conn.ID),
				slog.Int("chunk", index+1),
				slog.Int("chunks", len(requests)),
				slog.String("error", err.Error()))
			return false
		}
	}
	return true
}

// erasureTimeout bounds the deletion itself, which -- like the answer it
// precedes -- runs on the poller's single goroutine.
//
// It is much larger than commandAnswerTimeout because it covers a different
// kind of work: several bounded DELETEs and the unlinking of every attachment
// of one tenant. Blocking the poller for that long is acceptable precisely
// because the first step of the erasure disables the tenant's connections, so
// nothing of theirs is arriving meanwhile.
//
// Being cut short is not a corruption: the request stays 'consumed', and the
// same code resubmitted resumes the erasure where it stopped (cf.
// internal/erasure). That is what makes a ceiling here safe at all.
const erasureTimeout = 60 * time.Second

// handleDeleteMyData serves both halves of /delete_my_data: the bare command
// issues a confirmation code, the command followed by that code spends it.
//
// A confirmation reaches here through handleControlCommand, BEFORE the message
// is saved -- never through answerCommand, which only sees what the capture
// kept. That ordering is what keeps the code out of messages.text_content. A
// bare request on an enabled connection arrives through answerCommand instead,
// after the save, since it carries no secret.
//
// Either way, the sender IS the owner of the connection the message arrived
// through by the time this runs (control commands authenticate first, the
// answer path checks in answerCommand). A contact typing either form gets
// nothing at all -- not an answer in the chat, not an answer to themselves,
// and no erasure.
//
// Nothing is returned to the poller, for the same reason /privacy returns
// nothing: an undelivered answer must never make an update look like it
// failed. A failed erasure is logged and told to the owner, who can resume
// it with the same code.
func (h *Handler) handleDeleteMyData(ctx context.Context, conn *business.Connection, text string) {
	if h.eraser == nil {
		h.logger.Debug("erasure command ignored: no eraser configured",
			slog.String("business_connection_id", conn.ID))
		return
	}

	tenant := erasure.Tenant{
		OwnerUserID:          conn.OwnerUserID,
		OwnerTelegramUserID:  conn.OwnerTelegramUserID,
		BusinessConnectionID: conn.ID,
	}
	code := telegram.CommandArgument(text)
	if code == "" {
		h.requestErasure(ctx, conn, tenant)
		return
	}
	h.confirmErasure(ctx, conn, tenant, code)
}

// requestErasure issues the challenge and sends it to the owner.
//
// The code is only ever held in memory between these two lines: the database
// stores its hash, and no log line carries either. A challenge that cannot be
// delivered is left to expire on its own rather than being retracted -- the
// owner simply types the command again, and issuing a new code invalidates it.
func (h *Handler) requestErasure(ctx context.Context, conn *business.Connection, tenant erasure.Tenant) {
	requestCtx, cancel := context.WithTimeout(ctx, commandAnswerTimeout)
	defer cancel()

	challenge, err := h.eraser.Request(requestCtx, tenant)
	if err != nil {
		h.logger.Error("failed to issue an erasure challenge",
			slog.String("business_connection_id", conn.ID),
			slog.String("error", err.Error()))
		return
	}

	h.sendCommandAnswer(ctx, conn, "erasure challenge", []telegram.SendMessageRequest{
		telegram.BuildErasureChallengeRequest(conn.OwnerTelegramUserID, challenge.Code, erasure.ChallengeTTL),
	})
}

// confirmErasure spends a submitted code and answers what it did.
//
// Every outcome gets an answer, including the refusals: a code that expired
// while the owner was reading the message, or one mistyped, must not leave them
// wondering whether their data is gone.
func (h *Handler) confirmErasure(ctx context.Context, conn *business.Connection, tenant erasure.Tenant, code string) {
	eraseCtx, cancel := context.WithTimeout(ctx, erasureTimeout)
	defer cancel()

	outcome, err := h.eraser.Confirm(eraseCtx, tenant, code)
	if err != nil {
		// Logged with ids only, never the code: the erasure is resumable and
		// the owner is told so, but an operator reading this log must not be
		// handed a spendable token.
		h.logger.Error("erasure did not complete",
			slog.String("business_connection_id", conn.ID),
			slog.Int64("owner_user_id", conn.OwnerUserID),
			slog.String("error", err.Error()))
		h.sendCommandAnswer(ctx, conn, "erasure failure", []telegram.SendMessageRequest{
			telegram.BuildErasureNoticeRequest(conn.OwnerTelegramUserID, telegram.ErasureFailedNotice),
		})
		return
	}

	var request telegram.SendMessageRequest
	var what string
	switch outcome {
	case erasure.OutcomeErased:
		request = telegram.BuildErasureConfirmationRequest(conn.OwnerTelegramUserID, h.backupRetentionDays)
		what = "erasure confirmation"
	case erasure.OutcomeAlreadyErased:
		request = telegram.BuildErasureReplayRequest(conn.OwnerTelegramUserID, h.backupRetentionDays)
		what = "erasure replay"
	case erasure.OutcomeExpired:
		request = telegram.BuildErasureNoticeRequest(conn.OwnerTelegramUserID, telegram.ErasureExpiredNotice)
		what = "erasure expired code"
	default:
		request = telegram.BuildErasureNoticeRequest(conn.OwnerTelegramUserID, telegram.ErasureUnknownNotice)
		what = "erasure unknown code"
	}

	h.sendCommandAnswer(ctx, conn, what, []telegram.SendMessageRequest{request})
	h.logger.Info("erasure command answered",
		slog.String("business_connection_id", conn.ID),
		slog.String("answer", what))
}

// saveMedia catalogues the attachments of a message. The rows are created
// pending: the bytes are downloaded afterwards, by the media fetch loop, and
// only a stored row can end up in a deletion alert.
//
// file_index is the position in the list returned by ExtractMedia, which is
// deterministic for a given message: a Telegram redelivery therefore hits the
// upsert on the same (message, file_index) instead of duplicating the file.
func (h *Handler) saveMedia(ctx context.Context, ownerUserID int64, msg *telegram.Message, attachments []telegram.MediaAttachment) error {
	if h.media == nil {
		return nil
	}
	for index, attachment := range attachments {
		record := media.Record{
			BusinessConnectionID: msg.BusinessConnectionID,
			ChatID:               msg.Chat.ID,
			MessageID:            msg.MessageID,
			FileIndex:            index,
			TelegramFileID:       attachment.FileID,
			TelegramFileUniqueID: attachment.FileUniqueID,
			MediaType:            attachment.Type,
			MimeType:             attachment.MimeType,
			FileName:             attachment.FileName,
			MediaGroupID:         attachment.MediaGroupID,
			// Zero means "Telegram did not say", never "empty file": the
			// optional metadata stays NULL rather than being stored as 0.
			ByteSize:    optional(attachment.ByteSize),
			Width:       optional(attachment.Width),
			Height:      optional(attachment.Height),
			DurationSec: optional(attachment.DurationSec),
		}
		if _, err := h.media.Save(ctx, ownerUserID, record); err != nil {
			return fmt.Errorf("media catalogue: %w", err)
		}
	}
	return nil
}

// optional turns a Telegram optional numeric field into a nullable column.
func optional[T int | int64](value T) *T {
	if value == 0 {
		return nil
	}
	return &value
}

// messageType describes what was deleted. The type of the FIRST attachment
// wins: a Telegram message carries at most one media, an album being several
// messages that each keep their own type.
func messageType(attachments []telegram.MediaAttachment) string {
	if len(attachments) == 0 {
		return "text"
	}
	return attachments[0].Type
}

// messageText is what the sender wrote: text on a plain message, caption on a
// media message. Telegram never fills both.
func messageText(msg *telegram.Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	return msg.Caption
}

// handleDeleted resolves the connection, loops over message_ids (constraint
// #6) and delegates to messages.MarkDeleted, which sets deleted_at AND writes
// the alert chunks into notification_outbox within a single transaction.
//
// No Telegram call is made here since #27: delivery is asynchronous,
// handled by outbox.Worker. A nil return therefore means "the deletion is
// recorded and the alert is guaranteed to go out", not "the alert is out".
func (h *Handler) handleDeleted(ctx context.Context, del *telegram.BusinessMessagesDeleted) error {
	if del == nil {
		return nil
	}
	if del.BusinessConnectionID == "" || len(del.MessageIDs) == 0 || del.Chat.ID == 0 {
		h.logger.Debug("deletion ignored: empty connection, empty message_ids or zero chat_id",
			slog.Int("requested", len(del.MessageIDs)),
			slog.Int64("chat_id", del.Chat.ID))
		return nil
	}
	conn, err := h.business.Resolve(ctx, del.BusinessConnectionID)
	if err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			return nil
		}
		return fmt.Errorf("connection resolution for deletion: %w", err)
	}

	found, err := h.messages.MarkDeleted(ctx, conn.OwnerUserID, conn.OwnerTelegramUserID, del.BusinessConnectionID, del.Chat.ID, del.MessageIDs)
	if err != nil {
		return fmt.Errorf("deletion marking: %w", err)
	}

	foundIDs := make(map[int64]bool, len(found))
	for _, d := range found {
		foundIDs[d.MessageID] = true
	}

	// message_id missing from `found`: predates the Business connection, or
	// already purged by retention. Not an error -- log debug and keep going,
	// exactly as requested.
	for _, id := range del.MessageIDs {
		if !foundIDs[id] {
			h.logger.Debug("deleted message not found in database (predates the connection, or already purged)",
				slog.String("business_connection_id", del.BusinessConnectionID),
				slog.Int64("chat_id", del.Chat.ID),
				slog.Int64("message_id", id))
		}
	}

	// Aggregated counter: the number of messages actually recovered and
	// marked deleted. No id or text leaves here (cf. metrics).
	metrics.AddDeletions(int64(len(found)))

	h.logger.Info("deletion handled",
		slog.String("business_connection_id", del.BusinessConnectionID),
		slog.Int64("chat_id", del.Chat.ID),
		slog.Int("requested", len(del.MessageIDs)),
		slog.Int("recovered", len(found)))

	return nil
}

// chatTitle computes the display label of a chat. Telegram only fills title
// for chats that have one (groups, channels): a private chat is only
// described by first_name/last_name, which then become the label. A chat
// without any of these fields keeps no label -- the alert shows its id,
// never an invented value.
func chatTitle(c telegram.Chat) string {
	if c.Title != "" {
		return c.Title
	}
	name := c.FirstName
	if c.LastName != "" {
		if name != "" {
			name += " "
		}
		name += c.LastName
	}
	return name
}

func displayName(u *telegram.User) string {
	name := u.FirstName
	if u.LastName != "" {
		if name != "" {
			name += " "
		}
		name += u.LastName
	}
	if u.Username != "" {
		name += " (@" + u.Username + ")"
	}
	return name
}
