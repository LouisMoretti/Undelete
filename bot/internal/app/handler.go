// Package app assemble les dépendances et route chaque Update Telegram vers
// le traitement métier adéquat. C'est le seul endroit du code qui connaît
// la totalité du flux (résolution de connexion -> sauvegarde -> notification).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf16"

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
	client   *telegram.Client
	logger   *slog.Logger
}

func NewHandler(businessSvc *business.Service, messagesRepo *messages.Repository, client *telegram.Client, logger *slog.Logger) *Handler {
	return &Handler{
		business: businessSvc,
		messages: messagesRepo,
		client:   client,
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

	found, err := h.messages.MarkDeleted(ctx, conn.OwnerUserID, del.BusinessConnectionID, del.Chat.ID, del.MessageIDs)
	if err != nil {
		return fmt.Errorf("marquage suppression: %w", err)
	}

	foundIDs := make(map[int64]bool, len(found))
	for _, d := range found {
		foundIDs[d.MessageID] = true
		h.notifyDeletion(ctx, conn.OwnerTelegramUserID, d)
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

// notifyDeletion alerte le owner avec le contenu original retrouvé.
//
// Contrainte n°7 : chat_id = telegram_user_id du owner (chat privé
// bot<->owner), et surtout PAS de BusinessConnectionID -- ce message vient
// du bot lui-même, il ne doit jamais apparaître comme émis par le owner
// dans la conversation surveillée.
const telegramTextLimit = 4096

func (h *Handler) notifyDeletion(ctx context.Context, ownerTelegramUserID int64, d messages.DeletedRecord) {
	text := fmt.Sprintf("Message supprimé récupéré (chat %d) :\n\n%s", d.ChatID, d.TextContent)
	for i, chunk := range splitTelegramText(text, telegramTextLimit) {
		if err := h.client.SendMessage(ctx, telegram.SendMessageRequest{
			ChatID: ownerTelegramUserID,
			Text:   chunk,
		}); err != nil {
			// Le contenu et le chunk ne sont jamais logués. L'index suffit à
			// diagnostiquer une notification partiellement envoyée.
			h.logger.Error("échec notification suppression",
				slog.String("error", err.Error()),
				slog.Int("chunk_index", i))
			return
		}
	}
}

// splitTelegramText respecte la limite sendMessage en unités UTF-16. Compter
// les octets ou les runes ne suffit pas pour les caractères hors BMP (emoji),
// qui occupent deux unités UTF-16 côté Telegram. Le découpage conserve chaque
// rune entière et ne tronque jamais le contenu récupéré.
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
