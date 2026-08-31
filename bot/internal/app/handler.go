// Package app assemble les dépendances et route chaque Update Telegram vers
// le traitement métier adéquat. C'est le seul endroit du code qui connaît
// la totalité du flux (résolution de connexion -> sauvegarde -> notification).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// Handler route les updates Telegram Business vers le traitement métier.
// Ses méthodes sont appelées de façon strictement séquentielle par
// telegram.Poller (contrainte n°5) : aucune protection par mutex n'est
// nécessaire ici, l'ordre d'appel EST la garantie de cohérence.
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

// HandleUpdate implémente telegram.Handler.
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
		// allowed_updates ne demande que les 4 types business_* : un update
		// d'un autre type ne devrait jamais arriver ici. On logue et on
		// continue plutôt que de faire échouer le traitement.
		h.logger.Debug("update ignoré : aucun champ business_* peuplé", slog.Int64("update_id", u.UpdateID))
		return nil
	}
}

// saveMessage sauvegarde systématiquement le message reçu.
//
// Contrainte n°8 : AUCUNE condition ici sur chat_id, ni consultation d'une
// quelconque table de préférence -- une connexion Business active couvre
// automatiquement tous les chats que Telegram lui rend accessibles. Le seul
// filtre appliqué est celui de business.Service.Resolve (la connexion
// existe-t-elle et est-elle is_enabled), jamais un filtre par conversation.
func (h *Handler) saveMessage(ctx context.Context, msg *telegram.Message, edited bool) error {
	conn, err := h.business.Resolve(ctx, msg.BusinessConnectionID)
	if err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			h.logger.Debug("message ignoré : connexion refusée par le garde-fou mono-tenant",
				slog.String("business_connection_id", msg.BusinessConnectionID))
			return nil
		}
		return fmt.Errorf("résolution connexion pour sauvegarde: %w", err)
	}
	if !conn.IsEnabled {
		h.logger.Debug("message ignoré : connexion désactivée",
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
		MessageType:          "text", // Phase 1 : texte uniquement
		TextContent:          msg.Text,
		TelegramDate:         msg.Date,
	}

	if err := h.messages.Save(ctx, conn.OwnerUserID, record, edited); err != nil {
		return fmt.Errorf("sauvegarde message: %w", err)
	}

	// Logs : ids, types, compteurs uniquement. JAMAIS msg.Text ni aucun
	// contenu utilisateur -- contrainte produit, pas une préférence de
	// style : logger le contenu répliquerait toutes les conversations
	// surveillées dans les logs applicatifs.
	h.logger.Info("message sauvegardé",
		slog.String("business_connection_id", conn.ID),
		slog.Int64("chat_id", msg.Chat.ID),
		slog.Int64("message_id", msg.MessageID),
		slog.Bool("edited", edited))

	return nil
}

// handleDeleted résout la connexion, boucle sur message_ids (contrainte
// n°6), marque chaque message trouvé comme supprimé et notifie le owner.
func (h *Handler) handleDeleted(ctx context.Context, del *telegram.BusinessMessagesDeleted) error {
	conn, err := h.business.Resolve(ctx, del.BusinessConnectionID)
	if err != nil {
		if errors.Is(err, business.ErrOwnerMismatch) {
			return nil
		}
		return fmt.Errorf("résolution connexion pour suppression: %w", err)
	}

	found, err := h.messages.MarkDeleted(ctx, conn.OwnerUserID, conn.OwnerTelegramUserID, del.BusinessConnectionID, del.Chat.ID, del.MessageIDs)
	if err != nil {
		return fmt.Errorf("marquage suppression: %w", err)
	}

	foundIDs := make(map[int64]bool, len(found))
	for _, d := range found {
		foundIDs[d.MessageID] = true
	}

	// message_id absent de `found` : antérieur à la connexion Business, ou
	// déjà purgé par la rétention. Pas une erreur -- log debug et on
	// continue, exactement comme demandé.
	for _, id := range del.MessageIDs {
		if !foundIDs[id] {
			h.logger.Debug("message supprimé introuvable en base (antérieur à la connexion, ou déjà purgé)",
				slog.String("business_connection_id", del.BusinessConnectionID),
				slog.Int64("chat_id", del.Chat.ID),
				slog.Int64("message_id", id))
		}
	}

	h.logger.Info("suppression traitée",
		slog.String("business_connection_id", del.BusinessConnectionID),
		slog.Int64("chat_id", del.Chat.ID),
		slog.Int("requested", len(del.MessageIDs)),
		slog.Int("recovered", len(found)))

	return nil
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
