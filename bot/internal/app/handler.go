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

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// Handler routes Telegram Business updates to business handling. Its
// methods are called strictly sequentially by telegram.Poller (constraint
// #5): no mutex protection is needed here, the call order IS the
// consistency guarantee.
type Handler struct {
	business *business.Service
	messages *messages.Repository
	logger   *slog.Logger
}

func NewHandler(businessSvc *business.Service, messagesRepo *messages.Repository, logger *slog.Logger) *Handler {
	return &Handler{
		business: businessSvc,
		messages: messagesRepo,
		logger:   logger,
	}
}

// HandleUpdate implements telegram.Handler.
func (h *Handler) HandleUpdate(ctx context.Context, u telegram.Update) error {
	switch {
	case u.BusinessConnection != nil:
		return h.business.HandleBusinessConnection(ctx, *u.BusinessConnection)

	case u.BusinessMessage != nil:
		return h.saveMessage(ctx, u.BusinessMessage, false)

	case u.EditedBusinessMessage != nil:
		return h.saveMessage(ctx, u.EditedBusinessMessage, true)

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

// saveMessage always saves the received message.
//
// Constraint #8: NO chat_id condition here, and no consultation of any
// preference table -- an active Business connection automatically covers
// all chats Telegram exposes to it. The only filter applied is
// business.Service.Resolve (does the connection exist and is_enabled),
// never a per-conversation filter.
func (h *Handler) saveMessage(ctx context.Context, msg *telegram.Message, edited bool) error {
	conn, err := h.business.Resolve(ctx, msg.BusinessConnectionID)
	if err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			h.logger.Debug("message ignored: connection refused by the mono-tenant guard",
				slog.String("business_connection_id", msg.BusinessConnectionID))
			return nil
		}
		return fmt.Errorf("connection resolution for save: %w", err)
	}
	if !conn.IsEnabled {
		h.logger.Debug("message ignored: connection disabled",
			slog.String("business_connection_id", conn.ID))
		return nil
	}

	var fromUserID *int64
	fromDisplay := ""
	if msg.From != nil {
		id := msg.From.ID
		fromUserID = &id
		fromDisplay = displayName(msg.From)
	}

	record := messages.Record{
		BusinessConnectionID: msg.BusinessConnectionID,
		ChatID:               msg.Chat.ID,
		MessageID:            msg.MessageID,
		FromUserID:           fromUserID,
		FromDisplay:          fromDisplay,
		MessageType:          "text", // Phase 1: text only
		TextContent:          msg.Text,
		TelegramDate:         msg.Date,
		ChatTitle:            chatTitle(msg.Chat),
		ChatUsername:         msg.Chat.Username,
		ChatType:             msg.Chat.Type,
	}

	if err := h.messages.Save(ctx, conn.OwnerUserID, record, edited); err != nil {
		return fmt.Errorf("message save: %w", err)
	}

	// Logs: ids, types, counters only. NEVER msg.Text nor any user
	// content -- a product constraint, not a style preference: logging the
	// content would replicate every monitored conversation into the
	// application logs.
	h.logger.Info("message saved",
		slog.String("business_connection_id", conn.ID),
		slog.Int64("chat_id", msg.Chat.ID),
		slog.Int64("message_id", msg.MessageID),
		slog.Bool("edited", edited))

	return nil
}

// handleDeleted resolves the connection, loops over message_ids (constraint
// #6) and delegates to messages.MarkDeleted, which sets deleted_at AND writes
// the alert chunks into notification_outbox within a single transaction.
//
// No Telegram call is made here since #27: delivery is asynchronous,
// handled by outbox.Worker. A nil return therefore means "the deletion is
// recorded and the alert is guaranteed to go out", not "the alert is out".
func (h *Handler) handleDeleted(ctx context.Context, del *telegram.BusinessMessagesDeleted) error {
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
		name += " " + u.LastName
	}
	if u.Username != "" {
		name += " (@" + u.Username + ")"
	}
	return name
}
